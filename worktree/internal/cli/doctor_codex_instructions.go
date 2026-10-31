package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func checkDoctorCodexInstructions(id string, cfg workflowconfig.Config, lookupEnv func(string) string) []doctorCheck {
	var checks []doctorCheck
	for _, backend := range cfg.AgentBackendConfigs() {
		if backend.Kind != workflowconfig.AgentBackendCodex {
			continue
		}
		check := doctorCheck{Name: "Project " + id + " Codex instructions " + backend.ID, Status: doctorOK}
		lookup := os.LookupEnv
		if lookupEnv != nil {
			lookup = func(key string) (string, bool) { value := lookupEnv(key); return value, value != "" }
		}
		credential, err := codexCredentialPath(backend.Command, lookup, os.UserHomeDir)
		if err != nil {
			check.Status = doctorWarn
			check.Detail = "cannot resolve Codex instruction home: " + err.Error()
			checks = append(checks, check)
			continue
		}
		var visible []string
		for _, name := range []string{"AGENTS.override.md", "AGENTS.md"} {
			path := filepath.Join(filepath.Dir(credential), name)
			if _, err := os.Stat(path); err == nil {
				visible = append(visible, path)
			} else if !errors.Is(err, os.ErrNotExist) {
				visible = append(visible, fmt.Sprintf("%s (inspection failed: %v)", path, err))
			}
		}
		check.Detail = "no user-level Codex instruction files found; workers use an isolated Codex home"
		if len(visible) > 0 {
			check.Status = doctorWarn
			check.Detail = "user-level instructions would be inherited without worker home isolation: " + strings.Join(visible, ", ")
			check.Hint = "Detent excludes these files from worker Codex homes. Put unattended project guidance in repository AGENTS.md; restart older Detent instances to apply isolation."
		}
		checks = append(checks, check)
	}
	return checks
}
