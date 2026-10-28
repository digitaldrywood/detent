package runner

import (
	"testing"

	"go.uber.org/goleak"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestMain(m *testing.M) {
	if err := testenv.ClearGitEnvironment(); err != nil {
		panic(err)
	}
	goleak.VerifyTestMain(m)
}
