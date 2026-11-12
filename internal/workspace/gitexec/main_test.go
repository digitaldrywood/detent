package gitexec

import (
	"github.com/digitaldrywood/detent/internal/testenv"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if err := testenv.ClearGitEnvironment(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
