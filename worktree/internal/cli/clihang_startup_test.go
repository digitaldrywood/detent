package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestRunnerStartupRequiresHub(t *testing.T) {
	for _, test := range []struct {
		name     string
		headless bool
		isolated bool
		injected bool
	}{
		{name: "running"},
		{name: "headless", headless: true},
		{name: "isolated demo", isolated: true},
		{name: "headless isolated demo", headless: true, isolated: true},
		{name: "injected startup", injected: true},
		{name: "injected isolated startup", injected: true, isolated: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			port := 0
			cfg := BootConfig{Mode: BootModeRunning, Host: "127.0.0.1", Port: &port, Headless: test.headless,
				Global: globalconfig.Config{Path: filepath.Join(t.TempDir(), "global.yaml")}, Output: newBootOutput()}
			if test.isolated {
				cfg.Isolated = &IsolatedRuntimeInfo{}
			}
			start := startRunning
			if test.injected {
				start = func(ctx context.Context, cfg BootConfig) error {
					return startRunningWithDependencies(ctx, cfg, startRunningDependencies{})
				}
			}
			err := start(t.Context(), cfg)
			problem := ProblemForError(err)
			if err == nil || problem.Code != "validation" || problem.ExitCode != ExitValidation || !strings.Contains(err.Error(), "Runners require client.hub_url.") {
				t.Fatalf("startup without Hub = %v, problem = %+v", err, problem)
			}
			for _, hint := range []string{"self-hosted Hub", "enroll a runner", "client.hub_url"} {
				if !strings.Contains(problem.SuggestedFix, hint) {
					t.Errorf("startup hint = %q, want %q", problem.SuggestedFix, hint)
				}
			}
		})
	}
}
