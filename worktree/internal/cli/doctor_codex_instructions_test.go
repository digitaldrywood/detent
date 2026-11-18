package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func TestCheckDoctorCodexInstructions(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, file string
		configured bool
		want       doctorStatus
	}{
		{"absent", "", false, doctorOK},
		{"global", "AGENTS.md", false, doctorWarn},
		{"override", "AGENTS.override.md", false, doctorWarn},
		{"command home", "AGENTS.md", true, doctorWarn},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			if tt.file != "" {
				if err := os.WriteFile(filepath.Join(home, tt.file), []byte("Ask for confirmation"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			command := "codex app-server"
			envHome := home
			if tt.configured {
				command = "env CODEX_HOME='" + home + "' codex app-server"
				envHome = t.TempDir()
			}
			cfg := workflowconfig.Default()
			cfg.Codex.Command = command
			checks := checkDoctorCodexInstructions("test", cfg, func(key string) string {
				if key == "CODEX_HOME" {
					return envHome
				}
				return ""
			})
			if len(checks) != 1 || checks[0].Status != tt.want {
				t.Fatalf("checks = %#v", checks)
			}
			if tt.file != "" && !strings.Contains(checks[0].Detail, filepath.Join(home, tt.file)) {
				t.Fatalf("missing instruction path: %#v", checks[0])
			}
		})
	}
}
