package services

import (
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"text/template"

	"bytes"
	"io"

	"github.com/onsi/ginkgo"
	"github.com/pkg/errors"

	"github.com/solo-io/solo-kit/pkg/utils/log"
)

const (
	containerName = "e2e_envoy"
)

var adminPort = 19000

func (ei *EnvoyInstance) buildBootstrap() string {
	var b bytes.Buffer
	parsedTemplate.Execute(&b, ei)
	return b.String()
}

const envoyConfigTemplate = `
node:
 cluster: ingress
 id: {{.ID}}
 metadata:
  role: {{.Role}}

static_resources:
  clusters:
  - name: xds_cluster
    connect_timeout: 5.000s
    hosts:
    - socket_address:
        address: {{.GlooAddr}}
        port_value: {{.Port}}
    http2_protocol_options: {}
    type: STATIC
{{if .RatelimitAddr}}
  - name: ratelimit_cluster
    connect_timeout: 5.000s
    hosts:
    - socket_address:
        address: {{.RatelimitAddr}}
        port_value: {{.RatelimitPort}}
    http2_protocol_options: {}
    type: STATIC
{{end}}

dynamic_resources:
  ads_config:
    api_type: GRPC
    grpc_services:
    - envoy_grpc: {cluster_name: xds_cluster}
  cds_config:
    ads: {}
  lds_config:
    ads: {}
  
admin:
  access_log_path: /dev/null
  address:
    socket_address:
      address: 0.0.0.0
      port_value: {{.AdminPort}}

{{if .RatelimitAddr}}
rate_limit_service:
  grpc_service:
    envoy_grpc:
      cluster_name: ratelimit_cluster
{{end}}
`

var parsedTemplate = template.Must(template.New("bootstrap").Parse(envoyConfigTemplate))

type EnvoyFactory struct {
	envoypath string
	tmpdir    string
	useDocker bool
	instances []*EnvoyInstance
}

func NewEnvoyFactory() (*EnvoyFactory, error) {
	// if an envoy binary is explicitly specified
	// use it
	envoypath := os.Getenv("ENVOY_BINARY")
	if envoypath != "" {
		log.Printf("Using envoy from environment variable: %s", envoypath)
		return &EnvoyFactory{
			envoypath: envoypath,
		}, nil
	}

	// maybe it is in the path?!
	envoypath, err := exec.LookPath("envoy")
	if err == nil {
		log.Printf("Using envoy from PATH: %s", envoypath)
		return &EnvoyFactory{
			envoypath: envoypath,
		}, nil
	}

	switch runtime.GOOS {
	case "darwin":
		log.Printf("Using docker to run envoy")

		return &EnvoyFactory{useDocker: true}, nil
	case "linux":
		// try to grab one form docker...
		tmpdir, err := ioutil.TempDir(os.Getenv("HELPER_TMP"), "envoy")
		if err != nil {
			return nil, err
		}

		envoyImageTag := os.Getenv("ENVOY_IMAGE_TAG")
		if envoyImageTag == "" {
			envoyImageTag = "latest"
		}
		log.Printf("Using envoy docker image tag: %s", envoyImageTag)

		bash := fmt.Sprintf(`
set -ex
CID=$(docker run -d  soloio/envoy:%s /bin/bash -c exit)

# just print the image sha for repoducibility
echo "Using Envoy Image:"
docker inspect soloio/envoy:%s -f "{{.RepoDigests}}"

docker cp $CID:/usr/local/bin/envoy .
docker rm $CID
    `, envoyImageTag, envoyImageTag)
		scriptfile := filepath.Join(tmpdir, "getenvoy.sh")

		ioutil.WriteFile(scriptfile, []byte(bash), 0755)

		cmd := exec.Command("bash", scriptfile)
		cmd.Dir = tmpdir
		cmd.Stdout = ginkgo.GinkgoWriter
		cmd.Stderr = ginkgo.GinkgoWriter
		if err := cmd.Run(); err != nil {
			return nil, err
		}

		return &EnvoyFactory{
			envoypath: filepath.Join(tmpdir, "envoy"),
			tmpdir:    tmpdir,
		}, nil

	default:
		return nil, errors.New("Unsupported OS: " + runtime.GOOS)
	}
}

func (ef *EnvoyFactory) EnvoyPath() string {
	return ef.envoypath
}

func (ef *EnvoyFactory) Clean() error {
	if ef == nil {
		return nil
	}
	if ef.tmpdir != "" {
		os.RemoveAll(ef.tmpdir)
	}
	instances := ef.instances
	ef.instances = nil
	for _, ei := range instances {
		ei.Clean()
	}
	return nil
}

type EnvoyInstance struct {
	RatelimitAddr string
	RatelimitPort uint32
	ID            string
	Role          string
	envoypath     string
	envoycfg      string
	logs          *bytes.Buffer
	cmd           *exec.Cmd
	useDocker     bool
	GlooAddr      string // address for gloo and services
	Port          uint32
	AdminPort     int32
}

func (ef *EnvoyFactory) NewEnvoyInstance() (*EnvoyInstance, error) {
	adminPort = adminPort + 1

	gloo := "127.0.0.1"
	var err error

	if ef.useDocker {
		gloo, err = localAddr()
		if err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}

	ei := &EnvoyInstance{
		envoypath: ef.envoypath,
		useDocker: ef.useDocker,
		GlooAddr:  gloo,
		AdminPort: int32(adminPort),
	}
	ef.instances = append(ef.instances, ei)
	return ei, nil

}

func (ei *EnvoyInstance) RunWithId(id string) error {
	ei.ID = id
	ei.Role = "default~proxy"

	return ei.runWithPort(8081)
}

func (ei *EnvoyInstance) Run(port int) error {
	ei.Role = "default~proxy"

	return ei.runWithPort(uint32(port))
}
func (ei *EnvoyInstance) RunWithRole(role string, port int) error {
	ei.Role = role
	return ei.runWithPort(uint32(port))
}

/*
func (ei *EnvoyInstance) DebugMode() error {

	_, err := http.Get("http://localhost:19000/logging?level=debug")

	return err
}
*/
func (ei *EnvoyInstance) runWithPort(port uint32) error {
	if ei.ID == "" {
		ei.ID = "ingress~for-testing"
	}
	ei.Port = port

	ei.envoycfg = ei.buildBootstrap()
	if ei.useDocker {
		err := runContainer(ei.envoycfg)
		if err != nil {
			return err
		}
		return nil
	}

	args := []string{"--config-yaml", ei.envoycfg, "--v2-config-only", "--disable-hot-restart", "--log-level", "debug"}

	// run directly
	cmd := exec.Command(ei.envoypath, args...)

	buf := &bytes.Buffer{}
	ei.logs = buf
	w := io.MultiWriter(ginkgo.GinkgoWriter, buf)
	cmd.Stdout = w
	cmd.Stderr = w

	runner := Runner{Sourcepath: ei.envoypath, ComponentName: "ENVOY"}
	cmd, err := runner.run(cmd)
	if err != nil {
		return err
	}
	ei.cmd = cmd
	return nil
}

func (ei *EnvoyInstance) Binary() string {
	return ei.envoypath
}

func (ei *EnvoyInstance) LocalAddr() string {
	return ei.GlooAddr
}

func (ei *EnvoyInstance) Clean() error {
	http.Post(fmt.Sprintf("http://localhost:%d/quitquitquit", ei.AdminPort), "", nil)
	if ei.cmd != nil {
		ei.cmd.Process.Kill()
		ei.cmd.Wait()
	}

	if ei.useDocker {
		if err := stopContainer(); err != nil {
			return err
		}
	}
	return nil
}

func runContainer(cfgpath string) error {
	envoyImageTag := os.Getenv("ENVOY_IMAGE_TAG")
	if envoyImageTag == "" {
		envoyImageTag = "latest"
	}

	cfgDir := filepath.Dir(cfgpath)
	image := "soloio/envoy:" + envoyImageTag
	args := []string{"run", "-d", "--rm", "--name", containerName,
		"-v", cfgDir + ":/etc/config/",
		"-p", "8080:8080",
		"-p", "8443:8443",
		"-p", "19000:19000",
		image,
		"/usr/local/bin/envoy", "--v2-config-only", "--disable-hot-restart", "--log-level", "debug",
		"--config-yaml", cfgpath,
	}

	fmt.Fprintln(ginkgo.GinkgoWriter, args)
	cmd := exec.Command("docker", args...)
	cmd.Dir = cfgDir
	cmd.Stdout = ginkgo.GinkgoWriter
	cmd.Stderr = ginkgo.GinkgoWriter
	err := cmd.Run()
	if err != nil {
		return errors.Wrap(err, "Unable to start envoy container")
	}
	return nil
}

func stopContainer() error {
	cmd := exec.Command("docker", "stop", containerName)
	cmd.Stdout = ginkgo.GinkgoWriter
	cmd.Stderr = ginkgo.GinkgoWriter
	err := cmd.Run()
	if err != nil {
		return errors.Wrap(err, "Error stopping container "+containerName)
	}
	return nil
}

func localAddr() (string, error) {
	ip := os.Getenv("GLOO_IP")
	if ip != "" {
		return ip, nil
	}
	// go over network interfaces
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, i := range ifaces {
		if (i.Flags&net.FlagUp == 0) ||
			(i.Flags&net.FlagLoopback != 0) ||
			(i.Flags&net.FlagPointToPoint != 0) {
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			return "", err
		}
		for _, addr := range addrs {
			switch v := addr.(type) {
			case *net.IPNet:
				if v.IP.To4() != nil {
					return v.IP.String(), nil
				}
			case *net.IPAddr:
				if v.IP.To4() != nil {
					return v.IP.String(), nil
				}
			}
		}
	}
	return "", errors.New("unable to find Gloo IP")
}

func (ei *EnvoyInstance) Logs() string {
	return ei.logs.String()
}
