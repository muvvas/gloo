package kube

import (
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"os"
	"path/filepath"

	"github.com/solo-io/glue/internal/pkg/kube/upstream"
	"github.com/solo-io/glue/pkg/api/types/v1"
	clientset "github.com/solo-io/glue/pkg/platform/kube/crd/client/clientset/versioned"
	. "github.com/solo-io/glue/test/helpers"
	"k8s.io/api/extensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var _ = Describe("KubeSecretWatcher", func() {
	var (
		masterUrl, kubeconfigPath string
		mkb                       *MinikubeInstance
		namespace                 string
	)
	BeforeSuite(func() {
		namespace = RandString(8)
		mkb = NewMinikube(false, namespace)
		err := mkb.Setup()
		Must(err)
		kubeconfigPath = filepath.Join(os.Getenv("HOME"), ".kube", "config")
		masterUrl, err = mkb.Addr()
		Must(err)
	})
	AfterSuite(func() {
		mkb.Teardown()
	})
	Describe("controller", func() {
		var (
			ingressCvtr *ingressConverter
			kubeClient  kubernetes.Interface
			glueClient  clientset.Interface
		)
		BeforeEach(func() {
			cfg, err := clientcmd.BuildConfigFromFlags(masterUrl, kubeconfigPath)
			Must(err)

			ingressCvtr, err = NewIngressConverter(cfg, time.Second, make(chan struct{}))
			Must(err)

			kubeClient, err = kubernetes.NewForConfig(cfg)
			Must(err)

			glueClient, err = clientset.NewForConfig(cfg)
			Must(err)

			err = RegisterCrds(cfg)
			Must(err)
		})
		Context("an ingress is created without our ingress class", func() {
			var (
				createdIngress *v1beta1.Ingress
				err            error
			)
			BeforeEach(func() {
				serviceName := "somethingsomethingsomething"
				servicePort := intstr.FromInt(8080)

				// add an ingress
				ingress := &v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: "ingress-",
						Namespace:    namespace,
					},
					Spec: v1beta1.IngressSpec{
						Backend: &v1beta1.IngressBackend{
							ServiceName: serviceName,
							ServicePort: servicePort,
						},
					},
				}
				createdIngress, err = kubeClient.ExtensionsV1beta1().Ingresses(namespace).Create(ingress)
				Must(err)
			})
			AfterEach(func() {
				err = kubeClient.ExtensionsV1beta1().Ingresses(namespace).Delete(createdIngress.Name, nil)
				Must(err)
				time.Sleep(time.Second)
			})
			It("does not return an error", func() {
				select {
				case <-time.After(time.Second):
					// passed without error
				case err := <-ingressCvtr.Error():
					Expect(err).NotTo(HaveOccurred())
					Fail("err passed, but was nil")
				}
			})
			It("ignores the ingress", func() {
				upstreams, err := glueClient.GlueV1().Upstreams(namespace).List(metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(upstreams.Items).To(HaveLen(0))
				routes, err := glueClient.GlueV1().Routes(namespace).List(metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(routes.Items).To(HaveLen(0))
			})
		})
		Context("an ingress is created with a default backend", func() {
			var (
				createdIngress *v1beta1.Ingress
				err            error
			)
			BeforeEach(func() {
				serviceName := "somethingsomethingsomething"
				servicePort := intstr.FromInt(8080)

				// add an ingress
				ingress := &v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: "ingress-",
						Namespace:    namespace,
						Annotations:  map[string]string{"kubernetes.io/ingress.class": GlueIngressClass},
					},
					Spec: v1beta1.IngressSpec{
						Backend: &v1beta1.IngressBackend{
							ServiceName: serviceName,
							ServicePort: servicePort,
						},
					},
				}
				createdIngress, err = kubeClient.ExtensionsV1beta1().Ingresses(namespace).Create(ingress)
				Must(err)
				time.Sleep(time.Second)
			})
			AfterEach(func() {
				err = kubeClient.ExtensionsV1beta1().Ingresses(namespace).Delete(createdIngress.Name, nil)
				Must(err)
				time.Sleep(time.Second)
			})
			It("does not return an error", func() {
				select {
				case <-time.After(time.Second * 1):
					// passed without error
				case err := <-ingressCvtr.Error():
					Expect(err).NotTo(HaveOccurred())
					Fail("err passed, but was nil")
				}
			})
			It("creates the expected upstream for the ingress", func() {
				upstreams, err := glueClient.GlueV1().Upstreams(namespace).List(metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(upstreams.Items).To(HaveLen(1))
				us := upstreams.Items[0]
				Expect(us.Name).To(Equal(upstreamName(createdIngress.Name, *createdIngress.Spec.Backend)))
				Expect(us.Spec.Type).To(Equal(upstream.Kubernetes))
			})
			It("creates the expected route for the ingress", func() {
				glueRoute := v1.Route{
					Matcher: v1.Matcher{
						Path: v1.Path{
							Prefix: "/",
						},
					},
					Destination: v1.Destination{
						SingleDestination: v1.SingleDestination{
							UpstreamDestination: &v1.UpstreamDestination{
								UpstreamName: upstreamName(createdIngress.Name, *createdIngress.Spec.Backend),
							},
						},
					},
					Weight: defaultRouteWeight,
				}
				routes, err := glueClient.GlueV1().Routes(namespace).List(metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(routes.Items).To(HaveLen(1))
				route := routes.Items[0]
				Expect(route.Name).To(Equal(routeName(glueRoute)))
				Expect(v1.Route(route.Spec)).To(Equal(glueRoute))
			})
		})
		Context("an ingress is created with multiple rules", func() {
			var (
				createdIngress *v1beta1.Ingress
				err            error
			)
			BeforeEach(func() {
				// add an ingress
				ingress := &v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: "ingress-",
						Namespace:    namespace,
						Annotations:  map[string]string{"kubernetes.io/ingress.class": GlueIngressClass},
					},
					Spec: v1beta1.IngressSpec{
						Rules: []v1beta1.IngressRule{
							{
								Host: "host1",
								IngressRuleValue: v1beta1.IngressRuleValue{
									HTTP: &v1beta1.HTTPIngressRuleValue{
										Paths: []v1beta1.HTTPIngressPath{
											{
												Path: "/foo/bar",
												Backend: v1beta1.IngressBackend{
													ServiceName: "service1",
													ServicePort: intstr.FromInt(1234),
												},
											},
											{
												Path: "/foo/baz",
												Backend: v1beta1.IngressBackend{
													ServiceName: "service2",
													ServicePort: intstr.FromInt(3456),
												},
											},
										},
									},
								},
							},
							{
								Host: "host2",
								IngressRuleValue: v1beta1.IngressRuleValue{
									HTTP: &v1beta1.HTTPIngressRuleValue{
										Paths: []v1beta1.HTTPIngressPath{
											{
												Path: "/foo/bar",
												Backend: v1beta1.IngressBackend{
													ServiceName: "service3",
													ServicePort: intstr.FromInt(1234),
												},
											},
											{
												Path: "/straw/berry",
												Backend: v1beta1.IngressBackend{
													ServiceName: "service4",
													ServicePort: intstr.FromString("foo"),
												},
											},
											{
												Path: "/bat/girl",
												Backend: v1beta1.IngressBackend{
													ServiceName: "service4",
													ServicePort: intstr.FromString("foo"),
												},
											},
										},
									},
								},
							},
						},
					},
				}
				createdIngress, err = kubeClient.ExtensionsV1beta1().Ingresses(namespace).Create(ingress)
				Must(err)
				time.Sleep(time.Second * 2)
			})
			AfterEach(func() {
				err = kubeClient.ExtensionsV1beta1().Ingresses(namespace).Delete(createdIngress.Name, nil)
				Must(err)
			})
			It("does not return an error", func() {
				select {
				case <-time.After(time.Second * 2):
					// passed without error
				case err := <-ingressCvtr.Error():
					Expect(err).NotTo(HaveOccurred())
					Fail("err passed, but was nil")
				}
			})
			It("should de-duplicate repeated upstreams", func() {
				time.Sleep(time.Second * 3)
				expectedUpstreams := make(map[string]v1.Upstream)
				for _, rule := range createdIngress.Spec.Rules {
					for _, path := range rule.HTTP.Paths {
						expectedUpstreams[upstreamName(createdIngress.Name, path.Backend)] = v1.Upstream{
							Name: upstreamName(createdIngress.Name, path.Backend),
							Type: upstream.Kubernetes,
							Spec: upstream.ToMap(upstream.Spec{
								ServiceName:      path.Backend.ServiceName,
								ServiceNamespace: namespace,
								ServicePortName:  portName(path.Backend.ServicePort),
							}),
						}
					}
				}
				upstreams, err := glueClient.GlueV1().Upstreams(namespace).List(metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(upstreams.Items).To(HaveLen(len(expectedUpstreams)))
				for _, us := range upstreams.Items {
					Expect(expectedUpstreams).To(HaveKey(us.Name))
					Expect(us.Spec.Type).To(Equal(upstream.Kubernetes))
					Expect(expectedUpstreams[us.Name]).To(Equal(v1.Upstream(us.Spec)))
				}
			})
			It("create a route for every path", func() {
				time.Sleep(4 * time.Second)
				var expectedRoutes []v1.Route
				for _, rule := range createdIngress.Spec.Rules {
					for i, path := range rule.HTTP.Paths {
						expectedRoutes = append(expectedRoutes, v1.Route{
							Matcher: v1.Matcher{
								Path: v1.Path{
									Regex: path.Path,
								},
								VirtualHost: rule.Host,
							},
							Destination: v1.Destination{
								SingleDestination: v1.SingleDestination{
									UpstreamDestination: &v1.UpstreamDestination{
										UpstreamName: upstreamName(createdIngress.Name, path.Backend),
									},
								},
							},
							Weight: len(rule.IngressRuleValue.HTTP.Paths) - i,
						})
					}
				}
				routes, err := glueClient.GlueV1().Routes(namespace).List(metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(routes.Items).To(HaveLen(len(expectedRoutes)))
				for _, route := range routes.Items {
					Expect(expectedRoutes).To(ContainElement(v1.Route(route.Spec)))
				}
			})
		})
	})
})