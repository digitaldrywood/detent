package workspacerelay

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workspacehub"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestRelayFrameAuthorityDeadline(t *testing.T) {
	const budget = 25 * time.Second
	for _, tc := range []struct {
		name   string
		caller time.Duration
		want   time.Duration
	}{
		{name: "connection without a deadline", want: budget},
		{name: "caller deadline", caller: time.Minute, want: time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				if tc.caller > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.caller)
					defer cancel()
				}
				w := HTTP{HTTPDeps: HTTPDeps{
					RequestBudget: budget,
					Logger:        slog.New(slog.DiscardHandler),
					Repository: func() tenantstore.Repository {
						return relayDeadlineRepository{read: func(ctx context.Context) error {
							deadline, ok := ctx.Deadline()
							if !ok || time.Until(deadline) != tc.want {
								t.Fatalf("authority read deadline present = %t, time remaining = %s, want %s", ok, time.Until(deadline), tc.want)
							}
							<-ctx.Done()
							return ctx.Err()
						}}
					},
				}}
				connection := &Connection{Out: make(chan Outbound, 1), Done: make(chan struct{})}
				w.HandlePersonFrame(ctx, connection, workspacehub.Record{}, workspacesession.Frame{Type: workspacesession.TypeResume})
				select {
				case message := <-connection.Out:
					if message.Final != workspacesession.CodeRevoked {
						t.Errorf("connection close reason = %q, want %q", message.Final, workspacesession.CodeRevoked)
					}
				default:
					t.Error("authority deadline did not close the connection")
				}
			})
		})
	}
}

type relayDeadlineRepository struct {
	tenantstore.Repository
	read func(context.Context) error
}

func (r relayDeadlineRepository) ReadTenant(ctx context.Context, _ tracker.OrganizationID, _ corestore.Workload, _ func(tenantstore.Reader) error) error {
	return r.read(ctx)
}

func TestCloseRelayAuditDeadline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel bool
	}{
		{name: "active caller"},
		{name: "disconnected caller", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.cancel {
					cancel()
				}
				called := false
				w := HTTP{HTTPDeps: HTTPDeps{
					Clock: time.Now,
					AccessWrite: func(ctx context.Context, _ tracker.OrganizationID, _ func(tenantstore.Writer) error) error {
						called = true
						deadline, ok := ctx.Deadline()
						if !ok || time.Until(deadline) != hubapi.DefaultCleanupBudget {
							t.Fatalf("audit deadline present = %t, time remaining = %s, want %s", ok, time.Until(deadline), hubapi.DefaultCleanupBudget)
						}
						if err := ctx.Err(); err != nil {
							t.Fatalf("audit started with canceled context: %v", err)
						}
						<-ctx.Done()
						if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
							t.Errorf("audit context stopped with %v, want deadline exceeded", ctx.Err())
						}
						return nil
					},
				}}
				w.CloseRelayAudit(ctx, workspacehub.Record{}, &Connection{AuditID: "example"}, "person_closed")
				if !called {
					t.Error("audit cleanup did not reach the writer")
				}
			})
		})
	}
}

func TestReadOperatorFilesCallerDeadline(t *testing.T) {
	const budget = 30 * time.Second
	const readTime = 20 * time.Second
	for _, tc := range []struct {
		name                      string
		caller, cancelAt, elapsed time.Duration
		readErr                   error
	}{
		{name: "caller deadline", caller: budget, elapsed: readTime},
		{name: "configured budget", elapsed: readTime},
		{name: "caller cancellation", caller: budget, cancelAt: time.Second, elapsed: time.Second, readErr: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.caller > 0 {
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(ctx, tc.caller)
					defer stop()
				}
				started := time.Now()
				identity := operatortool.Identity{PrincipalID: "example", OrganizationID: "org_example", CredentialID: "key_example"}
				ctx = operatortool.WithConnection(ctx, operatortool.Connection{
					Identity: identity,
					Resolve: func(context.Context) (operatortool.Authority, error) {
						return operatortool.Authority{Identity: identity, Check: func(ctx context.Context, _ operatortool.Requirement) error {
							return ctx.Err()
						}}, nil
					},
				})
				record := workspacehub.Record{ID: "ws_example", OrganizationID: "org_example", ProjectID: "prj_example", State: workspacesession.StateReady,
					Capabilities: &workspacesession.Capabilities{Files: true}}
				relay := New(Deps{MemoryBytes: 1 << 20})
				runner := &Connection{WorkspaceID: record.ID, Out: make(chan Outbound, WriteQueue), Done: make(chan struct{})}
				if _, err := relay.AttachRunner(runner); err != nil {
					t.Fatal(err)
				}
				var reads int
				var firstReadErr error
				repository := operatorFileRepository{reader: operatorFileReader{read: func(ctx context.Context) (postgres.WorkspaceSession, error) {
					reads++
					if deadline, ok := ctx.Deadline(); !ok || !deadline.Equal(started.Add(budget)) {
						t.Errorf("workspace authorization deadline = %v, want %v", deadline, started.Add(budget))
					}
					if reads == 1 {
						if tc.cancelAt > 0 {
							timer := time.AfterFunc(tc.cancelAt, cancel)
							defer timer.Stop()
						}
						timer := time.NewTimer(readTime)
						defer timer.Stop()
						select {
						case <-ctx.Done():
							firstReadErr = ctx.Err()
							return postgres.WorkspaceSession{}, firstReadErr
						case <-timer.C:
						}
					}
					return postgres.WorkspaceSession{State: record.State}, nil
				}}}
				w := HTTP{HTTPDeps: HTTPDeps{
					RequestBudget:     budget,
					Relay:             relay,
					Clock:             time.Now,
					Logger:            slog.New(slog.DiscardHandler),
					RequireWorkspaces: func() error { return nil },
					Repository:        func() tenantstore.Repository { return repository },
					ReadOperatorWorkspace: func(ctx context.Context, _ hubscope.NativeScope, _ string) (workspacehub.Record, error) {
						if deadline, ok := ctx.Deadline(); !ok || !deadline.Equal(started.Add(budget)) {
							t.Errorf("workspace lookup deadline = %v, want %v", deadline, started.Add(budget))
						}
						return record, ctx.Err()
					},
					AccessWrite: func(context.Context, tracker.OrganizationID, func(tenantstore.Writer) error) error { return nil },
				}}
				payload, err := workspacesession.Encode(workspacesession.FilesContent{Path: "example.txt", Data: "example", Size: 7})
				if err != nil {
					t.Fatal(err)
				}
				runnerStopped := make(chan struct{})
				go func() {
					defer close(runnerStopped)
					select {
					case request := <-runner.Out:
						if request.Frame.Type != workspacesession.TypeFilesRead {
							t.Errorf("runner request type = %s, want file read", request.Frame.Type)
							return
						}
						w.HandleRunnerFrame(ctx, runner, workspacesession.Frame{
							Channel: workspacesession.ChannelFiles, Stream: request.Frame.Stream, Type: workspacesession.TypeFilesContent, Payload: payload,
						})
					case <-ctx.Done():
					}
				}()
				defer func() {
					cancel()
					<-runnerStopped
				}()
				result, err := w.ReadOperatorFiles(ctx, hubscope.NativeScope{Organization: record.OrganizationID}, "workspace_file_read",
					operatortool.WorkspaceArguments{WorkspaceID: record.ID, Path: "example.txt"})
				if elapsed := time.Since(started); elapsed != tc.elapsed || !errors.Is(firstReadErr, tc.readErr) {
					t.Errorf("file read took %s, workspace authorization returned %v, want %s and %v", elapsed, firstReadErr, tc.elapsed, tc.readErr)
				}
				if tc.readErr != nil {
					if len(result) != 0 || !errors.Is(err, workspacehub.ErrOperationUnavailable) || reads != 1 {
						t.Errorf("canceled file read returned data or continued authorization: bytes=%d err=%v reads=%d", len(result), err, reads)
					}
					return
				}
				var response struct {
					Status string
					Result workspacesession.FilesContent
				}
				if err != nil || json.Unmarshal(result, &response) != nil || response.Status != "available" || response.Result.Data != "example" || reads != 2 {
					t.Errorf("file read returned %s, err=%v, authority reads=%d", result, err, reads)
				}
			})
		})
	}
}

type operatorFileRepository struct {
	tenantstore.Repository
	reader operatorFileReader
}

func (r operatorFileRepository) ReadTenant(_ context.Context, _ tracker.OrganizationID, _ corestore.Workload, read func(tenantstore.Reader) error) error {
	return read(r.reader)
}

type operatorFileReader struct {
	tenantstore.Reader
	postgres.WorkspaceReader
	read func(context.Context) (postgres.WorkspaceSession, error)
}

func (r operatorFileReader) WorkspaceSession(ctx context.Context, _ tracker.ProjectID, _ string) (postgres.WorkspaceSession, error) {
	return r.read(ctx)
}

func (operatorFileReader) IsWorkspaceWorkItem(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (bool, error) {
	return false, nil
}
