package prefixrewrite_test

import (
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestPrefixrewrite(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Prefixrewrite Suite")
}
