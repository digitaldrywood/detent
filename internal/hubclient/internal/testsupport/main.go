package testsupport

import (
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func RunMain(m *testing.M, cleanups ...func() error) int {
	if err := testenv.ClearGitEnvironment(); err != nil {
		panic(err)
	}
	scratch, err := os.MkdirTemp("", "h")
	if err != nil {
		panic(err)
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		if err := os.Setenv(key, scratch); err != nil {
			panic(err)
		}
	}
	for key, value := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_AUTHOR_NAME": "Test User", "GIT_AUTHOR_EMAIL": "test@example.com", "GIT_COMMITTER_NAME": "Test User", "GIT_COMMITTER_EMAIL": "test@example.com"} {
		if err := os.Setenv(key, value); err != nil {
			panic(err)
		}
	}
	code := m.Run()
	for _, cleanup := range cleanups {
		if err := cleanup(); err != nil {
			panic(err)
		}
	}
	if err := os.RemoveAll(scratch); err != nil {
		panic(err)
	}
	return code
}
