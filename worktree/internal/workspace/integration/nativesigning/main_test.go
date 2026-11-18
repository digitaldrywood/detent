package nativesigning_test

import (
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspace/internal/gitfixture"
)

func TestMain(m *testing.M) {
	stubReapProcessScanner()
	os.Exit(gitfixture.RunTests(m))
}
