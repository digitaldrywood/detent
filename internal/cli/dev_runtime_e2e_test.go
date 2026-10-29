package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/devruntime"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestStartIsolatedRuntimeAutoPromotesFixtureAndStopsOnCancel(t *testing.T) {
	if testing.Short() {
		t.Skip("live service, profiling, or filesystem watcher integration")
	}

	runtime, err := devruntime.Build(devruntime.Config{Home: t.TempDir(), Port: 0})
	if err != nil {
		t.Fatalf("devruntime.Build() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	output := &lockedBuffer{}
	audits := make(chan isolatedAuditObservation, 1)
	cfg := devRuntimeBootConfig(t.Context(), runtime, "127.0.0.1", defaultOptions(), output)
	cfg.Runner = observedDevRuntimeRunner{audits: audits}
	done := make(chan error, 1)
	runtimeDone := make(chan struct{})
	go func() {
		done <- startRunning(ctx, cfg)
		close(runtimeDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runtimeDone:
		case <-time.After(10 * time.Second):
			t.Error("timed out waiting for isolated runtime cleanup")
		}
	})

	url := waitForIsolatedRuntimeURL(t, output, done)
	if banner := output.String(); !strings.Contains(banner, "Mode: isolated dev runtime") || !strings.Contains(banner, "DB mode: memory") {
		t.Fatalf("isolated runtime banner missing isolation details:\n%s", banner)
	}
	waitForDashboard(t, url+"/health", done)
	body := waitForDashboardConditionWithRefresh(t, url, url+"/api/v1/state", done, "mock issue promoted to Merging", func(body string) bool {
		return boardStateCountFromBody(t, body, "Merging") == 1
	})
	if !strings.Contains(body, `"status":"running"`) {
		t.Fatalf("state response missing running status:\n%s", body)
	}

	select {
	case audit := <-audits:
		if audit.request.Snapshot.BaseSHA != "isolated-mock-base" || audit.request.Snapshot.HeadSHA != "isolated-mock-head" || audit.execution.AuthenticationMode != "isolated_mock" || !strings.HasPrefix(audit.execution.InvocationID, "isolated-mock-") || !strings.Contains(audit.execution.Result.Summary, "no provider or worker process ran") {
			t.Fatalf("audit lacks exact synthetic source or provenance: %#v", audit)
		}
		for _, field := range []string{"base", "head", "diff", "pull request"} {
			request := audit.request
			switch field {
			case "base":
				request.Snapshot.BaseSHA = "other-base"
			case "head":
				request.Snapshot.HeadSHA = "other-head"
			case "diff":
				request.Snapshot.Diff = "other-diff"
			case "pull request":
				request.Snapshot.PRNumber = 9002
			}
			if _, err := (devRuntimeRunner{}).Audit(t.Context(), request); err == nil {
				t.Fatalf("isolated audit accepted mismatched %s", field)
			}
		}
	default:
		t.Fatal("mock issue promoted without executing its isolated security audit")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startRunning() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for isolated runtime to stop")
	}
}

func TestStartIsolatedRuntimePublishesBannerBeforeSnapshotLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("live service, profiling, or filesystem watcher integration")
	}

	for _, tt := range []struct {
		name string
		demo string
	}{
		{name: "fixture runtime"},
		{name: "screenshots demo", demo: devruntime.DemoScreenshots},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runtime, err := devruntime.Build(devruntime.Config{Home: t.TempDir(), Port: 0, Demo: tt.demo})
			if err != nil {
				t.Fatalf("devruntime.Build() error = %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			output := &lockedBuffer{}
			store := &bannerObservingBoardSnapshotStore{
				output: output, observed: make(chan string, 1), releaseLoad: make(chan struct{}),
			}
			done := make(chan error, 1)
			runtimeDone := make(chan struct{})
			go func() {
				done <- startRunningWithDependencies(ctx,
					devRuntimeBootConfig(t.Context(), runtime, "127.0.0.1", defaultOptions(), output),
					startRunningDependencies{boardSnapshotStore: store})
				close(runtimeDone)
			}()
			t.Cleanup(func() {
				cancel()
				store.release()
				select {
				case <-runtimeDone:
				case <-time.After(10 * time.Second):
					t.Error("timed out waiting for isolated runtime cleanup")
				}
			})

			var bannerAtLoad string
			select {
			case bannerAtLoad = <-store.observed:
			case err := <-done:
				t.Fatalf("isolated runtime stopped before snapshot load: %v", err)
			case <-time.After(bootDashboardURLTimeout):
				t.Fatal("timed out waiting for snapshot load")
			}

			url := waitForIsolatedRuntimeURL(t, output, done)
			if !strings.Contains(bannerAtLoad, "Dashboard: "+url) || !strings.Contains(bannerAtLoad, "Mode: isolated dev runtime") {
				t.Fatalf("snapshot load began before bound-address banner:\n%s", bannerAtLoad)
			}
			store.release()
			waitForDashboard(t, url+"/health", done)
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("startRunningWithDependencies() error = %v, want %v", err, context.Canceled)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("timed out waiting for isolated runtime to stop")
			}
		})
	}
}

type bannerObservingBoardSnapshotStore struct {
	output      *lockedBuffer
	observed    chan string
	releaseLoad chan struct{}
	releaseOnce sync.Once
}

func (s *bannerObservingBoardSnapshotStore) Load(context.Context) (telemetry.Snapshot, bool, error) {
	s.observed <- s.output.String()
	<-s.releaseLoad
	return telemetry.Snapshot{}, false, nil
}

func (*bannerObservingBoardSnapshotStore) Save(context.Context, telemetry.Snapshot) error {
	return nil
}

func (s *bannerObservingBoardSnapshotStore) release() {
	s.releaseOnce.Do(func() { close(s.releaseLoad) })
}

func TestDevRuntimeAuthConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		email        string
		smtp         string
		from         string
		issuer       string
		clientID     string
		clientSecret string
		wantMode     string
		wantErr      bool
	}{
		{name: "disabled"},
		{name: "magic link", email: "operator@example.com", smtp: "127.0.0.1:2525", from: "detent@example.com", wantMode: "magic_link"},
		{name: "oidc", email: "operator@example.com", issuer: "http://127.0.0.1:5555", clientID: "detent", clientSecret: "secret", wantMode: "oidc"},
		{name: "mixed providers", email: "operator@example.com", smtp: "127.0.0.1:2525", from: "detent@example.com", issuer: "http://127.0.0.1:5555", clientID: "detent", clientSecret: "secret", wantErr: true},
		{name: "incomplete oidc", email: "operator@example.com", issuer: "http://127.0.0.1:5555", clientID: "detent", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := devRuntimeAuthConfig(tt.email, tt.smtp, tt.from, tt.issuer, tt.clientID, tt.clientSecret)
			if (err != nil) != tt.wantErr {
				t.Fatalf("devRuntimeAuthConfig() error = %v, want error %t", err, tt.wantErr)
			}
			if cfg.Mode != tt.wantMode {
				t.Fatalf("devRuntimeAuthConfig().Mode = %q, want %q", cfg.Mode, tt.wantMode)
			}
		})
	}
}

func TestStartKanbanDemoRendersAndAppliesSafeActions(t *testing.T) {
	if testing.Short() {
		t.Skip("live service, profiling, or filesystem watcher integration")
	}

	const projectID = "demo-project"

	runtime, err := devruntime.Build(devruntime.Config{Home: t.TempDir(), Port: 0, Demo: devruntime.DemoKanban, DemoProjectID: projectID})
	if err != nil {
		t.Fatalf("devruntime.Build() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	output := &lockedBuffer{}
	done := make(chan error, 1)
	runtimeDone := make(chan struct{})
	go func() {
		done <- startRunning(ctx, devRuntimeBootConfig(t.Context(), runtime, "127.0.0.1", defaultOptions(), output))
		close(runtimeDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runtimeDone:
		case <-time.After(10 * time.Second):
			t.Error("timed out waiting for isolated runtime cleanup")
		}
	})

	dashboardURL := waitForIsolatedRuntimeURL(t, output, done)
	if banner := output.String(); !strings.Contains(banner, "Demo: kanban") {
		t.Fatalf("isolated runtime banner missing demo name:\n%s", banner)
	}
	waitForDashboard(t, dashboardURL+"/health", done)
	postRuntimeRefresh(t, dashboardURL, done)

	fleetBody := waitForDashboardCondition(t, dashboardURL+"/", done, "fleet board demo", func(body string) bool {
		return strings.Contains(body, `id="board-lanes"`) &&
			strings.Contains(body, `data-board-key="fleet"`) &&
			strings.Contains(body, `href="/projects/`+projectID+`/kanban"`) &&
			strings.Contains(body, "Kanban demo backlog intake") &&
			strings.Contains(body, "Kanban demo todo ready card")
	})
	for _, want := range []string{
		`data-kanban-action="move"`,
		`hx-post="/api/v1/kanban/move"`,
		`name="kanban_board" value="fleet"`,
	} {
		if !strings.Contains(fleetBody, want) {
			t.Fatalf("fleet board demo missing drag affordance %q:\n%s", want, fleetBody)
		}
	}

	pageURL := dashboardURL + "/projects/" + projectID + "/kanban"
	body := waitForDashboardCondition(t, pageURL, done, "project board demo", func(body string) bool {
		return strings.Contains(body, "Kanban demo backlog intake") &&
			strings.Contains(body, `data-board-key="project.`+projectID+`"`)
	})
	for _, want := range []string{"Backlog", "Todo", "In Progress", "Blocked", "Human Review", "Rework", "Merging", "Done", "Cancelled"} {
		if !strings.Contains(body, want) {
			t.Fatalf("kanban page missing lane %q:\n%s", want, body)
		}
	}

	sheetQuery := url.Values{
		"project": {projectID},
		"issue":   {"example-org/example-project#9511"},
		"scope":   {"project"},
		"actions": {"board"},
	}
	sheetURL := dashboardURL + "/api/v1/board/card?" + sheetQuery.Encode()
	waitForDashboardCondition(t, sheetURL, done, "board card detail sheet", func(body string) bool {
		return strings.Contains(body, "Kanban demo backlog intake") &&
			strings.Contains(body, `hx-get="/api/v1/kanban/move?`) &&
			strings.Contains(body, `data-detail-sheet`)
	})

	moveDialogValues := url.Values{
		"project_id":    {projectID},
		"issue_id":      {"kanban-demo-backlog"},
		"current_state": {"Backlog"},
		"target_state":  {"Todo"},
		"identifier":    {"example-org/example-project#9511"},
		"title":         {"Kanban demo backlog intake"},
	}
	moveDialogURL := dashboardURL + "/api/v1/kanban/move?" + moveDialogValues.Encode()
	waitForDashboardCondition(t, moveDialogURL, done, "kanban move dialog target", func(body string) bool {
		return strings.Contains(body, `hx-post="/api/v1/kanban/move"`) &&
			strings.Contains(body, `name="current_state" value="Backlog"`) &&
			strings.Contains(body, `<option value="Todo" selected>`)
	})

	moveBody := postRuntimeKanbanForm(t, dashboardURL+"/api/v1/kanban/move", done, url.Values{
		"project_id":    {projectID},
		"issue_id":      {"kanban-demo-backlog"},
		"current_state": {"Backlog"},
		"target_state":  {"Todo"},
	})
	for _, want := range []string{
		`id="board-lanes"`,
		"Moved card to Todo.",
		`id="card-example-org-example-project-9511"`,
	} {
		if !strings.Contains(moveBody, want) {
			t.Fatalf("immediate Kanban move response missing %q:\n%s", want, moveBody)
		}
	}
	if got := strings.Count(moveBody, `id="card-example-org-example-project-9511"`); got != 1 {
		t.Fatalf("immediate Kanban move response rendered moved card %d times, want 1:\n%s", got, moveBody)
	}
	postRuntimeRefresh(t, dashboardURL, done)
	movedCardPattern := regexp.MustCompile(`data-board-lane="todo"[\s\S]*?id="card-example-org-example-project-9511"`)
	waitForDashboardCondition(t, pageURL, done, "backlog card moved to todo", func(body string) bool {
		return movedCardPattern.MatchString(body)
	})

	postRuntimeKanbanForm(t, dashboardURL+"/api/v1/kanban/comment", done, url.Values{
		"project_id": {projectID},
		"target":     {"issue"},
		"issue_id":   {"kanban-demo-backlog"},
		"body":       {"Safe demo issue comment"},
	})
	postRuntimeKanbanForm(t, dashboardURL+"/api/v1/kanban/comment", done, url.Values{
		"project_id":    {projectID},
		"target":        {"pr"},
		"pr_repository": {"example-org/example-project"},
		"pr_number":     {"9515"},
		"body":          {"Safe demo PR comment"},
	})

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startRunning() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for isolated runtime to stop")
	}
}

type startupTranscriptSnapshot struct {
	store.Store
	snapshot func(context.Context)
}

func (s startupTranscriptSnapshot) ProtectedCodexThreadIDs(ctx context.Context) ([]string, error) {
	ids, err := s.Store.ProtectedCodexThreadIDs(ctx)
	if err == nil {
		s.snapshot(ctx)
	}
	return ids, err
}

func TestStartRunningServesWhileMaintenanceBlocked(t *testing.T) {
	if testing.Short() {
		t.Skip("live service, profiling, or filesystem watcher integration")
	}

	for _, outcome := range []string{"complete", "cancel", "required setup failure", "transcript cleanup"} {
		t.Run(outcome, func(t *testing.T) {
			complete := outcome == "complete" || outcome == "transcript cleanup"
			runtime, err := devruntime.Build(devruntime.Config{Home: t.TempDir(), Port: 0})
			if err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			maintenanceWorkflow := filepath.Join(home, "WORKFLOW.md")
			if err := os.WriteFile(maintenanceWorkflow, []byte("---\ncodex:\n  command: codex\n---\nMaintain artifacts.\n"), 0600); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			var paths, retained, expired []string
			for _, profile := range []string{workerCodexProfileDir, launchdCodexProfileDir} {
				paths = append(paths, writeCodexLogFixture(t, home, profile, now))
				retained = append(retained, writeRetentionRollout(t, filepath.Join(home, profile, "sessions"), profile+"-source", "", now.Add(-40*24*time.Hour)))
				expired = append(expired, writeRetentionRollout(t, filepath.Join(home, profile, "sessions"), profile+"-expired", "", now.Add(-40*24*time.Hour)))
			}
			ctx, cancel := context.WithCancel(t.Context())
			output := &lockedBuffer{}
			cfg := devRuntimeBootConfig(t.Context(), runtime, "127.0.0.1", defaultOptions(), output)
			if outcome == "required setup failure" {
				t.Setenv("DETENT_TEST_STARTUP_TOKEN", "")
				cfg.Global.Client = globalconfig.HubClient{URL: "http://127.0.0.1:1", TokenEnvironment: "DETENT_TEST_STARTUP_TOKEN"}
			}
			registry := project.NewRegistry()
			transcriptSnapshot := make(chan struct{})
			transcriptRelease := make(chan struct{})
			var transcriptOnce sync.Once
			finishTranscripts := func() { transcriptOnce.Do(func() { close(transcriptRelease) }) }
			entered := make(chan struct{})
			release := make(chan struct{})
			canceled := make(chan struct{})
			cleanupRelease := make(chan struct{})
			pruned := make(chan error, 1)
			var releaseOnce, cleanupOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			finishCleanup := func() { cleanupOnce.Do(func() { close(cleanupRelease) }) }
			done := make(chan error, 1)
			exited := make(chan struct{})
			go func() {
				done <- startRunningWithDependencies(ctx, cfg, startRunningDependencies{
					managerDependencies: project.ManagerDependencies{Registry: registry},
					startupMaintenance: func(ctx context.Context, maintenanceConfig globalconfig.Config, db store.Store) func() {
						for _, profile := range []string{workerCodexProfileDir, launchdCodexProfileDir} {
							source, err := db.StartSession(ctx, store.SessionStart{ProjectID: "retention", IssueID: profile, AgentBackendKind: "codex", ProviderThreadID: profile + "-source", StartedAt: now.Add(-40 * 24 * time.Hour)})
							if err == nil {
								err = db.FinishSession(ctx, source, store.SessionFinish{CompletedAt: now.Add(-39 * 24 * time.Hour)})
							}
							if err == nil {
								_, err = db.StartSession(ctx, store.SessionStart{ProjectID: "retention", IssueID: profile, AgentBackendKind: "codex", ProviderThreadID: profile + "-resumed", ResumedFromSessionID: source, StartedAt: now})
							}
							if err != nil {
								pruned <- err
								return nil
							}
						}
						threads := startupTranscriptSnapshot{Store: db, snapshot: func(ctx context.Context) {
							close(transcriptSnapshot)
							if outcome == "transcript cleanup" {
								select {
								case <-transcriptRelease:
								case <-ctx.Done():
								}
							}
						}}
						maintenanceConfig.Projects = []globalconfig.Project{{ID: "retention", Workdir: home, Workflow: maintenanceWorkflow}}
						maintainStartupCodexArtifacts(ctx, maintenanceConfig, threads)
						return func() {
							first := true
							pruned <- pruneCodexLogs(ctx, home, now, codexLogSizeLimit, func(ctx context.Context) (bool, error) {
								if first {
									first = false
									close(entered)
									select {
									case <-release:
									case <-ctx.Done():
										close(canceled)
										<-cleanupRelease
									}
								}
								return false, ctx.Err()
							})
						}
					},
				})
				close(exited)
			}()
			t.Cleanup(func() {
				cancel()
				finishTranscripts()
				unblock()
				finishCleanup()
				select {
				case <-exited:
				case <-time.After(10 * time.Second):
					t.Error("timed out joining startup runtime")
				}
			})
			dashboardURL := waitForIsolatedRuntimeURL(t, output, done)
			if outcome == "transcript cleanup" {
				select {
				case <-transcriptSnapshot:
				case <-time.After(10 * time.Second):
					t.Fatal("transcript protection snapshot did not start")
				}
				client := newRuntimeHTTPClient()
				defer client.CloseIdleConnections()
				observation, stopObservation := context.WithTimeout(ctx, 2*time.Second)
				defer stopObservation()
				ticks := time.NewTicker(10 * time.Millisecond)
				defer ticks.Stop()
				observed := false
				for observation.Err() == nil {
					request, err := http.NewRequestWithContext(observation, http.MethodGet, dashboardURL+"/health", nil)
					if err != nil {
						t.Fatal(err)
					}
					response, err := client.Do(request)
					if err == nil {
						observed = true
						if err := response.Body.Close(); err != nil {
							t.Fatal(err)
						}
						if response.StatusCode != http.StatusServiceUnavailable || len(registry.List()) != 0 {
							t.Fatalf("projects became available during transcript protection snapshot: health=%d projects=%d", response.StatusCode, len(registry.List()))
						}
					}
					select {
					case <-ticks.C:
					case <-observation.Done():
					}
				}
				if !observed {
					t.Fatal("health did not respond during transcript cleanup")
				}
				finishTranscripts()
			}
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("startup exited before maintenance: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("maintenance did not start")
			}
			if outcome != "required setup failure" {
				healthCtx, stopHealth := context.WithTimeout(ctx, 5*time.Second)
				defer stopHealth()
				waitForDashboardContext(t, healthCtx, dashboardURL+"/health", done)
				if len(registry.List()) == 0 {
					t.Fatal("ready without starting projects")
				}
				waitForDashboardConditionWithRefresh(t, dashboardURL, dashboardURL+"/api/v1/state", done, "authorized mock work advances during blocked retention", func(body string) bool {
					return boardStateCountFromBody(t, body, "Merging") == 1
				})
			}
			for _, path := range retained {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("resume source was removed: %v", err)
				}
			}
			for _, path := range expired {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("transcript cleanup did not finish before project startup: %v", err)
				}
			}
			if complete {
				unblock()
			} else {
				if outcome == "cancel" {
					cancel()
				}
				select {
				case <-canceled:
				case <-time.After(10 * time.Second):
					t.Fatal("maintenance did not observe cancellation")
				}
				select {
				case err := <-done:
					t.Fatalf("startup returned before maintenance joined: %v", err)
				default:
				}
				finishCleanup()
			}
			select {
			case err := <-pruned:
				if complete && err != nil || !complete && !errors.Is(err, context.Canceled) {
					t.Fatalf("retention result = %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("retention did not finish")
			}
			for _, path := range paths {
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				var rows int
				err = db.QueryRowContext(t.Context(), "SELECT count(*) FROM logs").Scan(&rows)
				if err := errors.Join(err, db.Close()); err != nil {
					t.Fatal(err)
				}
				want := 3
				if complete {
					want = 2
				}
				if rows != want {
					t.Fatalf("%s: rows = %d, want %d", path, rows, want)
				}
			}
			cancel()
			select {
			case err := <-done:
				if outcome == "required setup failure" {
					if err == nil || !strings.Contains(err.Error(), "DETENT_TEST_STARTUP_TOKEN is empty") || len(registry.List()) != 0 {
						t.Fatalf("missing Hub authority admitted projects: %v", err)
					}
				} else if !errors.Is(err, context.Canceled) {
					t.Fatalf("startup exit = %v, want cancellation", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("startup runtime did not exit")
			}
		})
	}
}

func TestStartRunningWaitsForBoardSnapshotPersistence(t *testing.T) {
	if testing.Short() {
		t.Skip("live service, profiling, or filesystem watcher integration")
	}

	runtime, err := devruntime.Build(devruntime.Config{Home: t.TempDir(), Port: 0, Demo: devruntime.DemoKanban})
	if err != nil {
		t.Fatalf("devruntime.Build() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	output := &lockedBuffer{}
	store := newBlockingBoardSnapshotStore()
	backgroundWaitStarted := make(chan struct{})
	runtimeExit := make(chan error, 1)
	done := make(chan struct{})
	var runErr error
	go func() {
		runErr = startRunningWithDependencies(
			ctx,
			devRuntimeBootConfig(t.Context(), runtime, "127.0.0.1", defaultOptions(), output),
			startRunningDependencies{
				boardSnapshotStore:    store,
				boardSnapshotInterval: time.Millisecond,
				backgroundWaitStarted: func() {
					close(backgroundWaitStarted)
				},
			},
		)
		runtimeExit <- runErr
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		store.release()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("timed out waiting for isolated runtime cleanup")
		}
	})

	dashboardURL := waitForIsolatedRuntimeURL(t, output, runtimeExit)
	waitForDashboard(t, dashboardURL+"/health", runtimeExit)
	postRuntimeRefresh(t, dashboardURL, runtimeExit)
	select {
	case <-store.started:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for board snapshot persistence")
	}

	cancel()
	select {
	case <-backgroundWaitStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for background shutdown")
	}
	select {
	case <-done:
		t.Fatalf("startRunningWithDependencies() returned before persistence completed: %v", runErr)
	default:
	}

	store.release()
	select {
	case <-done:
		if !errors.Is(runErr, context.Canceled) {
			t.Fatalf("startRunningWithDependencies() error = %v, want %v", runErr, context.Canceled)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for isolated runtime to stop")
	}
}

func TestStartScreenshotsDemoServesScenarioManifestAndUsage(t *testing.T) {
	if testing.Short() {
		t.Skip("live service, profiling, or filesystem watcher integration")
	}

	runtime, err := devruntime.Build(devruntime.Config{Home: t.TempDir(), Port: 0, Demo: devruntime.DemoScreenshots})
	if err != nil {
		t.Fatalf("devruntime.Build() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	output := &lockedBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- startRunning(ctx, devRuntimeBootConfig(t.Context(), runtime, "127.0.0.1", defaultOptions(), output))
	}()
	t.Cleanup(cancel)

	dashboardURL := waitForIsolatedRuntimeURL(t, output, done)
	if banner := output.String(); !strings.Contains(banner, "Demo: screenshots") || !strings.Contains(banner, "Scenario manifest: /api/v1/demo/scenarios") {
		t.Fatalf("isolated runtime banner missing screenshots metadata:\n%s", banner)
	}
	waitForDashboard(t, dashboardURL+"/health", done)

	manifest := waitForDashboard(t, dashboardURL+"/api/v1/demo/scenarios", done)
	if !strings.Contains(manifest, "fleet-healthy-parallel-work") || !strings.Contains(manifest, "reports-normal-window") {
		t.Fatalf("scenario manifest missing required scenarios:\n%s", manifest)
	}

	state := waitForDashboardHeader(t, dashboardURL+"/api/v1/state", done, "fleet-healthy-parallel-work")
	if !strings.Contains(state, `"status":"running"`) || !strings.Contains(state, `"issue_id":"demo-running-core"`) {
		t.Fatalf("scenario state response missing demo running issue:\n%s", state)
	}

	usage := waitForDashboardHeader(t, dashboardURL+"/api/v1/usage?by=project", done, "api-usage-populated")
	if !strings.Contains(usage, `"events":84`) || !strings.Contains(usage, `"bucket":"billing-api"`) {
		t.Fatalf("scenario usage response missing seeded ledger data:\n%s", usage)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startRunning() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for isolated runtime to stop")
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

type blockingBoardSnapshotStore struct {
	started     chan struct{}
	releaseSave chan struct{}
	startOnce   sync.Once
	releaseOnce sync.Once
}

func newBlockingBoardSnapshotStore() *blockingBoardSnapshotStore {
	return &blockingBoardSnapshotStore{
		started:     make(chan struct{}),
		releaseSave: make(chan struct{}),
	}
}

func (s *blockingBoardSnapshotStore) Load(context.Context) (telemetry.Snapshot, bool, error) {
	return telemetry.Snapshot{}, false, nil
}

func (s *blockingBoardSnapshotStore) Save(context.Context, telemetry.Snapshot) error {
	s.startOnce.Do(func() {
		close(s.started)
	})
	<-s.releaseSave
	return nil
}

func (s *blockingBoardSnapshotStore) release() {
	s.releaseOnce.Do(func() {
		close(s.releaseSave)
	})
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForIsolatedRuntimeURL(t *testing.T, output *lockedBuffer, done <-chan error) string {
	t.Helper()

	deadline := time.After(10 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("isolated runtime stopped before banner: %v", err)
		case <-deadline:
			t.Fatalf("timed out waiting for isolated runtime banner:\n%s", output.String())
		default:
		}

		for line := range strings.SplitSeq(output.String(), "\n") {
			url, ok := strings.CutPrefix(line, "Dashboard: ")
			if ok && strings.TrimSpace(url) != "" {
				return strings.TrimSpace(url)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForDashboardHeader(t *testing.T, rawURL string, done <-chan error, scenario string) string {
	t.Helper()

	client := newRuntimeHTTPClient()
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var lastErr error
	for ctx.Err() == nil {
		select {
		case err := <-done:
			t.Fatalf("isolated runtime stopped before dashboard response: %v", err)
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext() error = %v", err)
		}
		req.Header.Set("X-Detent-Demo-Scenario", scenario)
		resp, err := client.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil {
				t.Fatalf("ReadAll() error = %v", readErr)
			}
			if closeErr != nil {
				t.Fatalf("Body.Close() error = %v", closeErr)
			}
			if resp.StatusCode == http.StatusOK {
				return string(body)
			}
			lastErr = errors.New(resp.Status)
		} else {
			lastErr = err
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out fetching %s with scenario %s: %v", rawURL, scenario, lastErr)
	return ""
}

func postRuntimeRefresh(t *testing.T, url string, done <-chan error) {
	t.Helper()

	client := newRuntimeHTTPClient()
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for ctx.Err() == nil {
		select {
		case err := <-done:
			t.Fatalf("isolated runtime stopped before refresh: %v", err)
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/api/v1/refresh", nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext() error = %v", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			if closeErr := resp.Body.Close(); closeErr != nil {
				t.Fatalf("Body.Close() error = %v", closeErr)
			}
			if resp.StatusCode == http.StatusAccepted {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out posting runtime refresh to %s", url)
}

func waitForDashboardConditionWithRefresh(
	t *testing.T,
	refreshURL string,
	conditionURL string,
	done <-chan error,
	name string,
	ok func(string) bool,
) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	lastBody := ""
	nextRefresh := time.Time{}
	for ctx.Err() == nil {
		now := time.Now()
		if !now.Before(nextRefresh) {
			postRuntimeRefresh(t, refreshURL, done)
			nextRefresh = now.Add(time.Second)
		}
		body := waitForDashboardContext(t, ctx, conditionURL, done)
		lastBody = body
		if ok(body) {
			return body
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for dashboard condition %q at %s; last body:\n%s", name, conditionURL, lastBody)
	return ""
}

func postRuntimeKanbanForm(t *testing.T, rawURL string, done <-chan error, form url.Values) string {
	t.Helper()

	client := newRuntimeHTTPClient()
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for ctx.Err() == nil {
		select {
		case err := <-done:
			t.Fatalf("isolated runtime stopped before Kanban action: %v", err)
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatalf("NewRequestWithContext() error = %v", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		resp, err := client.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil {
				t.Fatalf("ReadAll() error = %v", readErr)
			}
			if closeErr != nil {
				t.Fatalf("Body.Close() error = %v", closeErr)
			}
			if resp.StatusCode == http.StatusOK {
				return string(body)
			}
			t.Fatalf("Kanban action status = %d, want 200; body = %s", resp.StatusCode, string(body))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out posting Kanban action to %s", rawURL)
	return ""
}

func boardStateCountFromBody(t *testing.T, body string, state string) int {
	t.Helper()

	var payload struct {
		Board struct {
			StateDistribution []struct {
				State string `json:"state"`
				Count int    `json:"count"`
			} `json:"state_distribution"`
		} `json:"board"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return 0
	}
	for _, entry := range payload.Board.StateDistribution {
		if entry.State == state {
			return entry.Count
		}
	}
	return 0
}

func newRuntimeHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{}, Timeout: time.Second}
}

type isolatedAuditObservation struct {
	request   orchestrator.SecurityAuditRequest
	execution orchestrator.SecurityAuditExecution
}

type observedDevRuntimeRunner struct {
	devRuntimeRunner
	audits chan isolatedAuditObservation
}

func (r observedDevRuntimeRunner) Audit(ctx context.Context, request orchestrator.SecurityAuditRequest) (orchestrator.SecurityAuditExecution, error) {
	execution, err := r.devRuntimeRunner.Audit(ctx, request)
	if err == nil {
		select {
		case r.audits <- isolatedAuditObservation{request: request, execution: execution}:
		default:
		}
	}
	return execution, err
}
