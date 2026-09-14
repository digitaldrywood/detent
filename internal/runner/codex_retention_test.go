package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
)

func TestRunnerReconcileWorkspacesPrunesRollouts(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "retention failure remains diagnostic"}[fail], func(t *testing.T) {
			called := false
			r := &Runner{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), workflow: config.Workflow{Config: config.Default()}, pruneRollouts: func(ctx context.Context, cfg config.Config) error {
				called = true
				if ctx != t.Context() {
					t.Error("wrong context")
				}
				if len(cfg.AgentBackendConfigs()) == 0 {
					t.Error("missing workflow")
				}
				if fail {
					return errors.New("retention unavailable")
				}
				return nil
			}}
			if _, err := r.ReconcileWorkspaces(t.Context(), nil); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("retention not called")
			}
		})
	}
}
