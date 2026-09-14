package runner

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestPreflightCleanupFailurePrecedence(t *testing.T) {
	t.Parallel()
	for _, role := range []string{RoleCode, RoleValidator, RoleSecurityAudit} {
		for _, failCleanup := range []bool{false, true} {
			name := role + "/selection_only"
			if failCleanup {
				name = role + "/selection_and_cleanup"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				var logs bytes.Buffer
				backend := &cleanupFailureBackend{t: t, failCleanup: failCleanup}
				cfg := config.Default()
				cfg.Agents.ModelSelection.Preset = new("sol_first")
				cfg.Gate.Validator.Model = "absent"
				cfg.Gate.SecurityAudit.Model = "absent"
				runner, err := NewRunner(Dependencies{
					Workflow:          config.Workflow{Config: cfg},
					Workspace:         &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir()}},
					AgentBackend:      backend,
					SecurityAuditRoot: t.TempDir(),
					Logger:            slog.New(slog.NewTextHandler(&logs, nil)),
				})
				if err != nil {
					t.Fatal(err)
				}
				issue := connector.Issue{
					ID: "issue-2561", Identifier: "digitaldrywood/detent#2561",
					Description: "```detent-agent\nschema: 1\nmodel: absent\n```",
				}
				switch role {
				case RoleCode:
					_, err = runner.Run(t.Context(), RunRequest{Issue: issue})
				case RoleValidator:
					_, err = runner.Validate(t.Context(), ValidatorRequest{Issue: issue})
				case RoleSecurityAudit:
					_, err = runner.Audit(t.Context(), SecurityAuditRequest{
						Issue: issue,
						Snapshot: securityaudit.Snapshot{
							Repository: "digitaldrywood/detent", PRNumber: 2564,
							BaseSHA: "base", HeadSHA: "head",
							Diff: "diff --git a/file b/file\n+change\n",
						},
					})
				}
				if err == nil || !backend.catalogCalled {
					t.Fatalf("error = %v, catalog called = %v", err, backend.catalogCalled)
				}
				var configurationErr *IssueConfigurationError
				if errors.As(err, &configurationErr) != !failCleanup {
					t.Fatalf("configuration classification = %v, want %v: %v", configurationErr, !failCleanup, err)
				}
				if failCleanup {
					if !strings.Contains(err.Error(), "cleanup agent preflight scratch") {
						t.Fatalf("missing cleanup failure: %v", err)
					}
					// The orchestrator also recognizes configuration errors by message text.
					if strings.Contains(err.Error(), "agent override rejected:") {
						t.Fatalf("cleanup failure retains configuration classification text: %v", err)
					}
					if !strings.Contains(logs.String(), "agent override rejected:") {
						t.Fatalf("selection diagnostic missing from logs: %s", logs.String())
					}
				}
			})
		}
	}
}

type cleanupFailureBackend struct {
	catalogAgentBackend
	t             *testing.T
	failCleanup   bool
	catalogCalled bool
}

func (b *cleanupFailureBackend) ListModels(_ context.Context, process AgentProcessRequest) ([]AgentModel, error) {
	b.catalogCalled = true
	if b.failCleanup {
		// Replace the attempt's scratch parent with a file to force a cleanup
		// error without permission assumptions, sleeps, or production test hooks.
		parent := filepath.Dir(process.TempDir)
		if err := os.RemoveAll(parent); err != nil {
			b.t.Fatal(err)
		}
		if err := os.WriteFile(parent, []byte("not a directory"), 0o600); err != nil {
			b.t.Fatal(err)
		}
	}
	return selectionCatalog(), nil
}
