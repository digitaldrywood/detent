package hubhealth

import (
	"fmt"
	"os"
	"testing"

	"go.uber.org/goleak"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestMain(m *testing.M) {
	if err := testenv.ClearGitEnvironment(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	goleak.VerifyTestMain(m)
}
