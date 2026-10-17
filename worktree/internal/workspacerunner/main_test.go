package workspacerunner_test

import (
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestMain(m *testing.M) {
	if err := testenv.ClearGitEnvironment(); err != nil {
		panic(err)
	}
	for key, value := range map[string]string{
		"GIT_CEILING_DIRECTORIES": os.Getenv("TMPDIR"),
		"GIT_CONFIG_GLOBAL":       os.DevNull,
		"GIT_CONFIG_NOSYSTEM":     "1",
	} {
		if err := os.Setenv(key, value); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}
