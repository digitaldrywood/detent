package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestMain(m *testing.M) {
	if err := testenv.ClearGitEnvironment(); err != nil {
		panic(err)
	}
	home, err := os.MkdirTemp("", "detent-doctor-codex-")
	if err != nil {
		panic(err)
	}
	for key, value := range map[string]string{
		"CODEX_HOME":          home,
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_NOSYSTEM": "1",
	} {
		if err := os.Setenv(key, value); err != nil {
			panic(err)
		}
	}
	code := m.Run()
	if err := os.RemoveAll(filepath.Clean(home)); err != nil {
		fmt.Fprintln(os.Stderr, "clean doctor test home:", err)
		code = 1
	}
	os.Exit(code)
}
