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
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/toolcache"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestPreflightCleanupFailurePrecedence(t *testing.T) {
	t.Parallel()
	t.Run("native missing workspace does not strand a reservation", func(t *testing.T) {
		ctx := workspace.WithValidationProcessOwner(t.Context(), func(context.Context, workspace.Info, workspace.Issue, string) (workspace.ValidationProcess, error) {
			t.Fatal("native external fallback created an unrecoverable workspace reservation")
			return nil, errors.New("unreachable")
		})
		process, cleanup, err := prepareAgentProcessRequest(ctx, AgentProcessRequest{Workspace: filepath.Join(t.TempDir(), "missing")}, workerGitHubPolicy{})
		if err != nil {
			t.Fatal(err)
		}
		if err := cleanup(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(process.TempDir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("external fallback scratch retained: %v", err)
		}
	})
	for _, role := range []string{RoleCode, RoleValidator, RoleSecurityAudit} {
		for _, failCleanup := range []bool{false, true} {
			name := role + "/selection_only"
			if failCleanup {
				name = role + "/selection_and_cleanup"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				var logs bytes.Buffer
				journal, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "runtime.db")})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = journal.Close() })
				cacheRoot := t.TempDir()
				cache, err := toolcache.NewManaged(t.Context(), cacheRoot, filepath.Join(cacheRoot, "runtime.db"))
				if err != nil {
					t.Fatal(err)
				}
				backend := &cleanupFailureBackend{t: t, failCleanup: failCleanup, journal: journal}
				cfg := config.Default()
				cfg.Agents.ModelSelection.Preset = new("sol_first")
				cfg.Agents.Routes = []config.AgentRoute{{Name: "default", Role: RoleCode, Backend: "codex", Model: "absent", Default: true}}
				cfg.Gate.Validator.Model = "absent"
				cfg.Gate.SecurityAudit.Model = "absent"
				runner, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource,
					BuildCache:        cache,
					Store:             journal,
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
					_, err = runner.Run(workspace.WithProcessSource(t.Context(), emptyProcessSource), RunRequest{Issue: issue})
				case RoleValidator:
					_, err = runner.Validate(workspace.WithProcessSource(t.Context(), emptyProcessSource), testValidatorRequest(issue))
				case RoleSecurityAudit:
					_, err = runner.Audit(workspace.WithProcessSource(t.Context(), emptyProcessSource), SecurityAuditRequest{
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
				processes, processErr := journal.ListActiveWorkerProcesses(t.Context())
				retained := 0
				if failCleanup {
					retained = 1
				}
				if processErr != nil || len(processes) != retained {
					t.Fatalf("preflight reservation after cleanup: %+v err=%v", processes, processErr)
				}
				var configurationErr *IssueConfigurationError
				if errors.As(err, &configurationErr) {
					t.Fatalf("configuration classification = %v, want %v: %v", configurationErr, false, err)
				}
				if failCleanup {
					if !strings.Contains(err.Error(), "cleanup agent preflight scratch") {
						t.Fatalf("missing cleanup failure: %v", err)
					}
					// The orchestrator also recognizes configuration errors by message text.
					if strings.Contains(err.Error(), "agent override rejected:") {
						t.Fatalf("cleanup failure retains configuration classification text: %v", err)
					}
					if !strings.Contains(logs.String(), "configured model") {
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
	journal       store.Store
}

func (b *cleanupFailureBackend) ListModels(ctx context.Context, process AgentProcessRequest) ([]AgentModel, error) {
	b.catalogCalled = true
	if b.journal != nil {
		processes, err := b.journal.ListActiveWorkerProcesses(ctx)
		if err != nil || len(processes) != 1 || processes[0].Role != store.WorkerProcessRoleHostValidation || processes[0].CleanupPath != process.TempDir {
			b.t.Fatalf("preflight started without durable scratch reservation: %+v err=%v", processes, err)
		}
	}
	if b.failCleanup {
		if err := os.RemoveAll(process.TempDir); err != nil {
			b.t.Fatal(err)
		}
		outside := b.t.TempDir()
		if err := os.Symlink(outside, process.TempDir); err != nil {
			b.t.Fatal(err)
		}
	}
	return selectionCatalog(), nil
}
