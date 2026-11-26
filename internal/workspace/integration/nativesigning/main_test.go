package nativesigning_test

import (
	"flag"
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspace/internal/gitfixture"
)

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(0)
	}
	stubReapProcessScanner()
	os.Exit(gitfixture.RunTests(m))
}
