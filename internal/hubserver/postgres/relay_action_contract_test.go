package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type relayActionQueryTrace struct {
	pgstoretest.ReportCountingTracer
	query string
	args  []any
}

func (trace *relayActionQueryTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "-- name: RecentActionRuns") {
		trace.query, trace.args = data.SQL, append([]any(nil), data.Args...)
	}
	return trace.ReportCountingTracer.TraceQueryStart(ctx, conn, data)
}

func TestRelayActionContractRedeemsOnceAndFencesOrderedReports(t *testing.T) {
	admin, repository, pool, now := workspaceContractFixture(t)
	objects := pgstoretest.NewHistoryObjects()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		command, err := objects.StoreDetail(t.Context(), string(org), []byte("example command"))
		if err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= 2; i++ {
			project := tracker.ProjectID(fmt.Sprintf("prj_%d", i))
			workspace := fmt.Sprintf("ws_%032x", i)
			if err := repository.WriteRelayAction(t.Context(), org, corestore.Serving, func(w postgres.RelayActionWriter) error {
				return w.CreateProjectAction(t.Context(), actionstore.Record{Action: workspacesession.Action{ID: "action", OrganizationID: string(org), ProjectID: string(project), Name: "Example", CreatedBy: "example", Revision: 1, CreatedAt: now, UpdatedAt: now}, Command: command})
			}); err != nil {
				t.Fatal(err)
			}
			err := repository.WriteRelayAction(t.Context(), org, corestore.Serving, func(w postgres.RelayActionWriter) error {
				if _, err := w.CreateRelayTicket(t.Context(), postgres.CreateRelayTicketInput{OrganizationID: org, ProjectID: project, Digest: fmt.Sprintf("ticket_%d", i), WorkspaceID: workspace, SessionID: "session", PrincipalID: "person", Subject: "subject", CreatedAt: now, ExpiresAt: now.Add(time.Minute)}); err != nil {
					return err
				}
				if _, err := w.RedeemRelayTicket(t.Context(), postgres.RedeemRelayTicketInput{OrganizationID: org, ProjectID: project, Digest: fmt.Sprintf("ticket_%d", i), SessionID: "session", PrincipalID: "person", Subject: "subject", Now: now}); err != nil {
					return err
				}
				if _, err := w.CreateRelaySession(t.Context(), postgres.CreateRelaySessionInput{OrganizationID: org, ProjectID: project, WorkspaceID: workspace, ID: "relay", PrincipalID: "person", Subject: "subject", ConnectionID: fmt.Sprintf("connection_%d", i), Channels: []string{"terminal"}, OpenedAt: now}); err != nil {
					return err
				}
				if _, err := w.PutTerminalRecording(t.Context(), postgres.PutTerminalRecordingInput{OrganizationID: org, ProjectID: project, WorkspaceID: workspace, ID: "recording", RelaySessionID: "relay", StreamID: "stream", PrincipalID: "person", Subject: "subject", Isolation: "user", StartedAt: now, TerminalCols: 80, TerminalRows: 24, CastObjectKey: command.Key, CastSha256: command.SHA256, CastBytes: command.CompressedSize, CastContentBytes: command.UncompressedSize, UpdatedAt: now}); err != nil {
					return err
				}
				request := postgres.CreateActionRunInput{OrganizationID: org, ProjectID: project, ID: "run", ActionID: "action", WorkspaceID: workspace, CreatedBy: "person", CreatedAt: now}
				run, err := w.CreateActionRun(t.Context(), request)
				if err != nil {
					return err
				}
				if run.CommandObjectKey != command.Key {
					t.Fatalf("command snapshot=%+v", run)
				}
				replay, err := w.CreateActionRun(t.Context(), request)
				if err != nil {
					return err
				}
				if replay.ID != run.ID || replay.Revision != 1 {
					t.Fatalf("run replay=%+v", replay)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			var session postgres.WorkspaceSession
			if err := repository.ReadWorkspace(t.Context(), org, corestore.Serving, func(r postgres.WorkspaceReader) error {
				var err error
				session, err = r.WorkspaceSession(t.Context(), project, workspace)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			claim := postgres.ClaimActionRunInput{OrganizationID: org, ProjectID: project, ID: "run", RunnerID: session.RunnerID, LeaseID: session.LeaseID, FencingToken: session.FencingToken, StartedAt: new(now)}
			if err := repository.WriteRelayAction(t.Context(), org, corestore.Serving, func(w postgres.RelayActionWriter) error { _, err := w.ClaimActionRun(t.Context(), claim); return err }); err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteRelayAction(t.Context(), org, corestore.Serving, func(w postgres.RelayActionWriter) error { _, err := w.ClaimActionRun(t.Context(), claim); return err }); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("duplicate claim=%v", err)
			}
			report := postgres.ReportActionRunInput{OrganizationID: org, ProjectID: project, ID: "run", RunnerID: session.RunnerID, LeaseID: session.LeaseID, FencingToken: session.FencingToken, Status: "running", OutputSequence: 1, UpdatedAt: now, OutputObjectKey: new(command.Key), OutputSha256: command.SHA256, OutputBytes: command.CompressedSize, OutputContentBytes: command.UncompressedSize}
			if err := repository.WriteRelayAction(t.Context(), org, corestore.Serving, func(w postgres.RelayActionWriter) error { _, err := w.ReportActionRun(t.Context(), report); return err }); err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name   string
				mutate func(*postgres.ReportActionRunInput)
			}{
				{"duplicate sequence", func(*postgres.ReportActionRunInput) {}},
				{"stale fence", func(r *postgres.ReportActionRunInput) { r.OutputSequence = 2; r.FencingToken++ }},
				{"wrong runner", func(r *postgres.ReportActionRunInput) { r.OutputSequence = 2; r.RunnerID = "foreign" }},
			} {
				t.Run(string(org)+fmt.Sprint(i)+test.name, func(t *testing.T) {
					input := report
					test.mutate(&input)
					err := repository.WriteRelayAction(t.Context(), org, corestore.Serving, func(w postgres.RelayActionWriter) error { _, err := w.ReportActionRun(t.Context(), input); return err })
					if !errors.Is(err, corestore.ErrRevisionConflict) {
						t.Fatalf("report=%v", err)
					}
				})
			}
			report.OutputSequence = 2
			report.Status = "succeeded"
			report.ExitCode = pgtype.Int4{Int32: 0, Valid: true}
			report.FinishedAt = new(now)
			if err := repository.WriteRelayAction(t.Context(), org, corestore.Serving, func(w postgres.RelayActionWriter) error { _, err := w.ReportActionRun(t.Context(), report); return err }); err != nil {
				t.Fatal(err)
			}
		}
	}
	closeCases := []struct {
		organization tracker.OrganizationID
		project      tracker.ProjectID
		workspace    string
		connection   string
		artifact     string
		wantArtifact string
	}{
		{"org_first", "prj_1", fmt.Sprintf("ws_%032x", 1), "connection_1", "", "recording-existing"},
		{"org_first", "prj_2", fmt.Sprintf("ws_%032x", 2), "connection_2", "recording-explicit", "recording-explicit"},
		{"org_second", "prj_1", fmt.Sprintf("ws_%032x", 1), "connection_1", "", "recording-existing"},
		{"org_second", "prj_2", fmt.Sprintf("ws_%032x", 2), "connection_2", "recording-explicit", "recording-explicit"},
	}
	type expectedRelay struct {
		artifact string
		closed   bool
	}
	expected := map[string]expectedRelay{}
	closedAt := now.Add(time.Minute)
	assertRelays := func() {
		t.Helper()
		for _, target := range closeCases {
			if err := repository.ReadRelayAction(t.Context(), target.organization, corestore.Serving, func(r postgres.RelayActionReader) error {
				rows, err := r.RelaySessions(t.Context(), postgres.RelaySessionsInput{OrganizationID: target.organization, ProjectID: target.project, WorkspaceID: target.workspace, Limit: 2})
				if err != nil {
					return err
				}
				if len(rows) != 1 {
					t.Fatalf("%s/%s relay count=%d, want1", target.organization, target.project, len(rows))
				}
				row := rows[0]
				want := expected[string(target.organization)+"/"+string(target.project)]
				if row.OrganizationID != target.organization || row.ProjectID != target.project || row.WorkspaceID != target.workspace || row.ID != "relay" || row.PrincipalID != "person" || row.Subject != "subject" || row.ConnectionID != target.connection || !row.OpenedAt.Equal(now) || len(row.Channels) != 1 || row.Channels[0] != "terminal" || row.RecordingArtifact != want.artifact || (row.ClosedAt != nil) != want.closed {
					t.Fatalf("scoped relay changed: organization=%s project=%s row=%+v expected=%+v", target.organization, target.project, row, want)
				}
				if want.closed {
					if !row.ClosedAt.Equal(closedAt) || row.CloseReason != "person_closed" || row.BytesIn != 11 || row.BytesOut != 17 {
						t.Fatalf("closed relay accounting=%+v", row)
					}
				} else if row.CloseReason != "" || row.BytesIn != 0 || row.BytesOut != 0 {
					t.Fatalf("unaffected open relay accounting=%+v", row)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	assertRelays()
	for _, target := range closeCases {
		result, err := admin.ExecContext(t.Context(), `UPDATE workspace_relay_sessions SET recording_artifact=$1 WHERE organization_id=$2 AND project_id=$3 AND workspace_id=$4 AND id='relay' AND connection_id=$5`, "recording-existing", target.organization, target.project, target.workspace, target.connection)
		if err != nil {
			t.Fatal(err)
		}
		if rows, err := result.RowsAffected(); err != nil || rows != 1 {
			t.Fatalf("scoped recording fixture rows=%d error=%v, want1", rows, err)
		}
		expected[string(target.organization)+"/"+string(target.project)] = expectedRelay{artifact: "recording-existing"}
		assertRelays()
	}
	for _, target := range closeCases {
		input := postgres.CloseRelaySessionInput{OrganizationID: target.organization, ProjectID: target.project, ID: "relay", ConnectionID: "other_connection", ClosedAt: &closedAt, CloseReason: "person_closed", BytesIn: 11, BytesOut: 17, RecordingArtifact: target.artifact}
		if err := repository.WriteRelayAction(t.Context(), target.organization, corestore.Serving, func(w postgres.RelayActionWriter) error { return w.CloseRelaySession(t.Context(), input) }); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("wrong connection close=%v", err)
		}
		assertRelays()
		input.ConnectionID = target.connection
		if err := repository.WriteRelayAction(t.Context(), target.organization, corestore.Serving, func(w postgres.RelayActionWriter) error { return w.CloseRelaySession(t.Context(), input) }); err != nil {
			t.Fatal(err)
		}
		expected[string(target.organization)+"/"+string(target.project)] = expectedRelay{artifact: target.wantArtifact, closed: true}
		assertRelays()
		input.CloseReason, input.BytesIn, input.BytesOut, input.RecordingArtifact = "later_close", 99, 101, "later_recording"
		input.ClosedAt = new(closedAt.Add(time.Minute))
		if err := repository.WriteRelayAction(t.Context(), target.organization, corestore.Serving, func(w postgres.RelayActionWriter) error { return w.CloseRelaySession(t.Context(), input) }); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("already closed relay=%v", err)
		}
		assertRelays()
	}
	for _, test := range []struct {
		name string
		arg  postgres.RedeemRelayTicketInput
		want error
	}{
		{"redeemed", postgres.RedeemRelayTicketInput{OrganizationID: "org_first", ProjectID: "prj_1", Digest: "ticket_1", SessionID: "session", PrincipalID: "person", Subject: "subject", Now: now}, corestore.ErrRevisionConflict},
		{"foreign tenant", postgres.RedeemRelayTicketInput{OrganizationID: "org_second", ProjectID: "prj_1", Digest: "ticket_1", Now: now}, corestore.ErrTenantMismatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := repository.WriteRelayAction(t.Context(), "org_first", corestore.Serving, func(w postgres.RelayActionWriter) error {
				_, err := w.RedeemRelayTicket(t.Context(), test.arg)
				return err
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("ticket=%v", err)
			}
		})
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository, pool = pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.ReadRelayAction(t.Context(), org, corestore.Serving, func(r postgres.RelayActionReader) error {
			for i := 1; i <= 2; i++ {
				project := tracker.ProjectID(fmt.Sprintf("prj_%d", i))
				workspace := fmt.Sprintf("ws_%032x", i)
				rows, err := r.RelaySessions(t.Context(), postgres.RelaySessionsInput{OrganizationID: org, ProjectID: project, WorkspaceID: workspace, Limit: 2})
				if err != nil {
					return err
				}
				if len(rows) != 1 || rows[0].OrganizationID != org {
					t.Fatalf("relays=%+v", rows)
				}
				wantArtifact := "recording-existing"
				if i == 2 {
					wantArtifact = "recording-explicit"
				}
				if rows[0].ClosedAt == nil || !rows[0].ClosedAt.Equal(closedAt) || rows[0].CloseReason != "person_closed" || rows[0].BytesIn != 11 || rows[0].BytesOut != 17 || rows[0].RecordingArtifact != wantArtifact {
					t.Fatalf("reopened relay close lost accounting or artifact: %+v", rows[0])
				}
				casts, err := r.TerminalRecordings(t.Context(), postgres.TerminalRecordingsInput{OrganizationID: org, ProjectID: project, WorkspaceID: workspace, Limit: 2})
				if err != nil {
					return err
				}
				if len(casts) != 1 || casts[0].OrganizationID != org || casts[0].CastObjectKey == "" {
					t.Fatalf("recordings=%+v", casts)
				}
				run, err := r.ActionRun(t.Context(), postgres.ActionRunInput{OrganizationID: org, ProjectID: project, ID: "run"})
				if err != nil {
					return err
				}
				if run.OrganizationID != org || run.Status != "succeeded" || run.OutputSequence != 2 || !run.ExitCode.Valid || run.ExitCode.Int32 != 0 {
					t.Fatalf("run=%+v", run)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("relay claims preserve original lease authority after rebind", func(t *testing.T) {
		current := time.Now().UTC().Truncate(time.Microsecond)
		trace := new(relayActionQueryTrace)
		counter := &trace.ReportCountingTracer
		tracePool := pgstoretest.TracedFeaturePool(t, pool, trace)
		counted, err := postgres.NewRepository(t.Context(), tracePool, pgstoretest.SmallPool())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := counted.Close(); err != nil {
				t.Error(err)
			}
		})
		type target struct {
			organization tracker.OrganizationID
			project      tracker.ProjectID
			workspace    postgres.WorkspaceSession
			lease        leasestore.Lease
			runner       string
		}
		var targets []target
		readRun := func(t *testing.T, v target, id string) postgres.ActionRun {
			t.Helper()
			var row postgres.ActionRun
			if err := repository.ReadRelayAction(t.Context(), v.organization, corestore.Serving, func(r postgres.RelayActionReader) error {
				var err error
				row, err = r.ActionRun(t.Context(), postgres.ActionRunInput{OrganizationID: v.organization, ProjectID: v.project, ID: id})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return row
		}
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			for i := 1; i <= 2; i++ {
				v := target{organization: org, project: tracker.ProjectID(fmt.Sprintf("prj_%d", i)), runner: fmt.Sprintf("relay_runner_%d", i)}
				pgstoretest.SeedRunnerIdentity(t, repository, org, v.runner, fmt.Sprintf("machine_%d", i), current, false)
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					owner := w.(postgres.WorkspaceWriter)
					var err error
					v.workspace, err = owner.WorkspaceSession(t.Context(), v.project, fmt.Sprintf("ws_%032x", i))
					if err != nil {
						return err
					}
					actions := w.(postgres.RelayActionWriter)
					if _, err := actions.CreateActionRun(t.Context(), postgres.CreateActionRunInput{OrganizationID: org, ProjectID: v.project, ID: "machine_only", ActionID: "action", WorkspaceID: v.workspace.ID, CreatedBy: "example", CreatedAt: current}); err != nil {
						return err
					}
					_, err = actions.ClaimActionRun(t.Context(), postgres.ClaimActionRunInput{OrganizationID: org, ProjectID: v.project, ID: "machine_only", RunnerID: v.workspace.RunnerID, LeaseID: v.workspace.LeaseID, FencingToken: v.workspace.FencingToken, StartedAt: &current, Claimant: "relayconn_example:machine"})
					if !errors.Is(err, corestore.ErrRevisionConflict) {
						t.Fatalf("unattributed stream claim=%v", err)
					}
					if _, err := w.Release(t.Context(), leasestore.Fence{LeaseID: v.workspace.LeaseID, FencingToken: v.workspace.FencingToken}, "example"); err != nil {
						return err
					}
					item, err := w.WorkItem(t.Context(), v.project, tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", i)))
					if err != nil {
						return err
					}
					v.lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: v.workspace.MachineID, RunnerID: v.runner, SessionID: "relay_session_" + string(v.project), LeaseID: "relay_lease_" + string(v.project), TTL: time.Hour})
					if err != nil {
						return err
					}
					row := v.workspace
					v.workspace, err = owner.UpdateWorkspaceSession(t.Context(), postgres.WorkspaceUpdate{OrganizationID: org, ProjectID: v.project, ID: row.ID, ExpectedRevision: row.Revision, ExpectedRunnerID: row.RunnerID, ExpectedLeaseID: row.LeaseID, ExpectedFencingToken: row.FencingToken, State: "ready", RunnerID: v.runner, MachineID: row.MachineID, LeaseID: v.lease.LeaseID, FencingToken: v.lease.FencingToken, Capabilities: []string{"files", "exec"}, UpdatedAt: current})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if row := readRun(t, v, "machine_only"); row.Status != "queued" || row.Revision != 1 || row.ClaimedBy != "" {
					t.Fatalf("refused stream changed row=%+v", row)
				}
				targets = append(targets, v)
			}
		}
		claim := func(v target, id string, at time.Time) postgres.ActionRun {
			t.Helper()
			var row postgres.ActionRun
			if err := repository.WriteRelayAction(t.Context(), v.organization, corestore.Serving, func(w postgres.RelayActionWriter) error {
				if _, err := w.CreateActionRun(t.Context(), postgres.CreateActionRunInput{OrganizationID: v.organization, ProjectID: v.project, ID: id, ActionID: "action", WorkspaceID: v.workspace.ID, CreatedBy: "person", CreatedAt: at}); err != nil {
					return err
				}
				var err error
				row, err = w.ClaimActionRun(t.Context(), postgres.ClaimActionRunInput{OrganizationID: v.organization, ProjectID: v.project, ID: id, RunnerID: v.runner, LeaseID: v.lease.LeaseID, FencingToken: v.lease.FencingToken, StartedAt: &current, Claimant: "relayconn_example:" + id})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if row.ClaimedBy != "relayconn_example:"+id || row.Status != "running" || row.Revision != 2 {
				t.Fatalf("stream claim=%+v", row)
			}
			return row
		}
		key := func(v target) string { return string(v.organization) + "/" + string(v.project) }
		normal := make(map[string]postgres.ActionRun)
		for _, v := range targets {
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO project_action_runs(organization_id,project_id,id,action_id,workspace_id,command_object_key,command_sha256,command_bytes,command_content_bytes,status,exit_code,started_at,finished_at,claimed_by,lease_id,fencing_token,created_by,created_at,updated_at)
SELECT a.organization_id,a.project_id,'history_'||lpad(n::text,9,'0'),a.id,$3,a.command_object_key,a.command_sha256,a.command_bytes,a.command_content_bytes,'succeeded',0,$4::timestamptz-n*interval '1 second',$4::timestamptz-n*interval '1 second','history',$5,$6,'example',$4::timestamptz-n*interval '1 second',$4::timestamptz-n*interval '1 second'
FROM project_actions a CROSS JOIN generate_series(1,10000) n WHERE a.organization_id=$1 AND a.project_id=$2 AND a.id='action'`, v.organization, v.project, v.workspace.ID, current, v.lease.LeaseID, v.lease.FencingToken); err != nil {
				t.Fatal(err)
			}
			if err := counted.WriteRelayAction(t.Context(), v.organization, corestore.Serving, func(w postgres.RelayActionWriter) error {
				for _, id := range []string{"recent_a", "recent_z"} {
					if _, err := w.CreateActionRun(t.Context(), postgres.CreateActionRunInput{OrganizationID: v.organization, ProjectID: v.project, ID: id, ActionID: "action", WorkspaceID: v.workspace.ID, CreatedBy: "example", CreatedAt: current}); err != nil {
						return err
					}
				}
				rows, err := w.RecentActionRuns(t.Context(), v.project, "action", nil, 2)
				if err != nil {
					return err
				}
				if len(rows) != 2 || rows[0].ID != "recent_z" || rows[1].ID != "recent_a" {
					t.Fatalf("newest-first ties=%+v", rows)
				}
				beforeSeek := counter.Count.Load()
				rows, err = w.RecentActionRuns(t.Context(), v.project, "action", &postgres.ActionRunCursor{CreatedAt: current, ID: "recent_a"}, 1)
				if got := counter.Count.Load() - beforeSeek; got != 2 {
					t.Fatalf("recent-run seek and selected hydration statements=%d want2", got)
				}
				if err != nil {
					return err
				}
				if len(rows) != 1 || rows[0].ID != "machine_only" {
					t.Fatalf("descending seek=%+v", rows)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if trace.query == "" || len(trace.args) == 0 {
				t.Fatal("actual recent-run seek was not captured")
			}
			for _, phase := range []string{"fresh", "analyzed"} {
				if phase == "analyzed" {
					if _, err := admin.ExecContext(t.Context(), "ANALYZE project_action_runs"); err != nil {
						t.Fatal(err)
					}
				}
				tx := pgstoretest.TenantTx(t, pgstoretest.RuntimeFor(t, tracePool, tracePool), tracePool, string(v.organization))
				var raw []byte
				if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+trace.query, trace.args...).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				if err := tx.Rollback(t.Context()); err != nil {
					t.Fatal(err)
				}
				type planNode struct {
					Kind      string     `json:"Node Type"`
					Index     string     `json:"Index Name"`
					Condition string     `json:"Index Cond"`
					Rows      float64    `json:"Actual Rows"`
					Loops     float64    `json:"Actual Loops"`
					Removed   float64    `json:"Rows Removed by Filter"`
					Plans     []planNode `json:"Plans"`
				}
				var plans []struct{ Plan planNode }
				if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
					t.Fatalf("decode recent-run plan=%v", err)
				}
				found := false
				var inspect func(planNode)
				inspect = func(node planNode) {
					if strings.Contains(node.Kind, "Sort") {
						t.Fatalf("%s recent-run seek sorted history: %s", phase, raw)
					}
					if node.Index == "action_run_recent_order" {
						found = true
						if node.Rows != 1 || (node.Rows+node.Removed)*node.Loops > 1 || !strings.Contains(node.Condition, "created_at") || !strings.Contains(node.Condition, "id") {
							t.Fatalf("unbounded or vacuous recent-run seek=%+v", node)
						}
					}
					for _, child := range node.Plans {
						inspect(child)
					}
				}
				inspect(plans[0].Plan)
				if !found {
					t.Fatal("recent-run seek did not use its scoped ordering index")
				}
			}
			normal[key(v)] = claim(v, "normal_exit", current)
			for _, id := range []string{"lease_lost", "stream_closed", "workspace_closed", "retained_dispatch", "rebind_exit"} {
				claim(v, id, current)
			}
		}
		assertNormal := func(t *testing.T) {
			t.Helper()
			for _, v := range targets {
				if got := readRun(t, v, "normal_exit"); !reflect.DeepEqual(got, normal[key(v)]) {
					t.Fatalf("cross-scope terminal write: scope=%s got=%+v want=%+v", key(v), got, normal[key(v)])
				}
			}
		}
		for _, v := range targets {
			output, err := objects.StoreDetail(t.Context(), string(v.organization), []byte("example output"))
			if err != nil {
				t.Fatal(err)
			}
			completeCast, err := objects.StoreDetail(t.Context(), string(v.organization), []byte("example output with the complete second teardown"))
			if err != nil {
				t.Fatal(err)
			}
			input := func(row postgres.ActionRun) postgres.FinishRelayActionRunInput {
				return postgres.FinishRelayActionRunInput{OrganizationID: v.organization, ProjectID: v.project, ID: row.ID, WorkspaceID: row.WorkspaceID, RunnerID: &v.runner, LeaseID: row.LeaseID, FencingToken: row.FencingToken, Claimant: row.ClaimedBy, ExpectedRevision: row.Revision, ExpectedOutputSequence: row.OutputSequence, Status: "succeeded", ExitCode: pgtype.Int4{Int32: 0, Valid: true}, FinishedAt: current.Add(time.Second), OutputObjectKey: &output.Key, OutputSha256: output.SHA256, OutputBytes: output.CompressedSize, OutputContentBytes: output.UncompressedSize}
			}
			finish := func(t *testing.T, arg postgres.FinishRelayActionRunInput, want error) postgres.ActionRun {
				t.Helper()
				var row postgres.ActionRun
				counter.Count.Store(0)
				err := counted.WriteRelayAction(t.Context(), v.organization, corestore.Serving, func(w postgres.RelayActionWriter) error {
					var err error
					before := counter.Count.Load()
					row, err = w.FinishRelayActionRun(t.Context(), arg)
					wantStatements := int64(1)
					if errors.Is(want, corestore.ErrInvalidPage) || errors.Is(want, corestore.ErrTenantMismatch) {
						wantStatements = 0
					}
					if got := counter.Count.Load() - before; got != wantStatements {
						t.Fatalf("finish owner statements=%d want=%d", got, wantStatements)
					}
					return err
				})
				if !errors.Is(err, want) {
					t.Fatalf("finish error=%v want=%v input=%+v", err, want, arg)
				}
				return row
			}

			if err := repository.WriteRelayAction(t.Context(), v.organization, corestore.Serving, func(w postgres.RelayActionWriter) error {
				command, err := w.ProjectAction(t.Context(), v.project, "action")
				if err != nil {
					return err
				}
				command.Action.ID = "automatic"
				command.Action.RunOnWorktreeCreation = true
				if err := w.CreateProjectAction(t.Context(), command); err != nil {
					return err
				}
				created, err := w.CreateReportedActionRun(t.Context(), postgres.CreateReportedActionRunInput{OrganizationID: v.organization, ProjectID: v.project, ID: "reported_run", ActionID: "automatic", WorkspaceID: v.workspace.ID, RunnerID: v.runner, LeaseID: v.lease.LeaseID, FencingToken: v.lease.FencingToken, Status: "running", OutputObjectKey: &output.Key, OutputSha256: output.SHA256, OutputBytes: output.CompressedSize, OutputContentBytes: output.UncompressedSize, CreatedAt: current})
				if err != nil {
					return err
				}
				if created.Revision != 1 || created.Status != "running" || created.ClaimedBy != "runner:"+v.runner || created.StartedAt != nil || created.CreatedBy != "" {
					t.Fatalf("first worker report=%+v", created)
				}
				apply := postgres.ApplyReportedActionRunInput{OrganizationID: v.organization, ProjectID: v.project, ID: created.ID, ActionID: created.ActionID, WorkspaceID: v.workspace.ID, RunnerID: v.runner, LeaseID: v.lease.LeaseID, FencingToken: v.lease.FencingToken, ExpectedRevision: 1, Status: "running", Truncated: true, UpdatedAt: current.Add(time.Second)}
				updated, err := w.ApplyReportedActionRun(t.Context(), apply)
				if err != nil {
					return err
				}
				if updated.Revision != 2 || updated.OutputObjectKey == nil || *updated.OutputObjectKey != output.Key || updated.Truncated || updated.StartedAt != nil {
					t.Fatalf("omitted worker output changed stored receipt=%+v", updated)
				}
				apply.ExpectedRevision = 2
				apply.Status = "failed"
				apply.ExitCode = pgtype.Int4{Int32: 17, Valid: true}
				apply.FinishedAt = new(current.Add(time.Second))
				terminal, err := w.ApplyReportedActionRun(t.Context(), apply)
				if err != nil {
					return err
				}
				if terminal.Revision != 3 || !terminal.ExitCode.Valid || terminal.ExitCode.Int32 != 17 || terminal.Reason != "" || terminal.Status != "failed" {
					t.Fatalf("terminal worker report=%+v", terminal)
				}
				apply.ExpectedRevision = 3
				apply.Status = "running"
				if _, err := w.ApplyReportedActionRun(t.Context(), apply); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("terminal worker rewrite=%v", err)
				}
				apply.ID = "normal_exit"
				apply.ActionID = "action"
				apply.ExpectedRevision = 2
				apply.Status = "running"
				apply.ExitCode = pgtype.Int4{}
				apply.FinishedAt = nil
				if _, err := w.ApplyReportedActionRun(t.Context(), apply); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("worker took stream-owned run=%v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			assertNormal(t)
			base := input(normal[key(v)])
			for _, test := range []struct {
				name   string
				mutate func(*postgres.FinishRelayActionRunInput)
				want   error
			}{
				{"running status", func(a *postgres.FinishRelayActionRunInput) { a.Status = "running" }, corestore.ErrInvalidPage},
				{"queued status", func(a *postgres.FinishRelayActionRunInput) { a.Status = "queued" }, corestore.ErrInvalidPage},
				{"exit with teardown reason", func(a *postgres.FinishRelayActionRunInput) { a.Status = "failed"; a.Reason = "stream_closed" }, corestore.ErrInvalidPage},
				{"success without exit", func(a *postgres.FinishRelayActionRunInput) { a.ExitCode = pgtype.Int4{} }, corestore.ErrInvalidPage},
				{"cross stream", func(a *postgres.FinishRelayActionRunInput) { a.Claimant = "other_stream" }, corestore.ErrRevisionConflict},
				{"wrong lease", func(a *postgres.FinishRelayActionRunInput) { a.LeaseID = "other_lease" }, corestore.ErrRevisionConflict},
				{"stale fence", func(a *postgres.FinishRelayActionRunInput) { a.FencingToken++ }, corestore.ErrRevisionConflict},
				{"wrong workspace", func(a *postgres.FinishRelayActionRunInput) { a.WorkspaceID = "ws_missing" }, corestore.ErrRevisionConflict},
				{"wrong project", func(a *postgres.FinishRelayActionRunInput) { a.ProjectID = "prj_missing" }, corestore.ErrRevisionConflict},
				{"wrong organization", func(a *postgres.FinishRelayActionRunInput) { a.OrganizationID = "org_missing" }, corestore.ErrTenantMismatch},
				{"stale revision", func(a *postgres.FinishRelayActionRunInput) { a.ExpectedRevision-- }, corestore.ErrRevisionConflict},
				{"wrong sequence", func(a *postgres.FinishRelayActionRunInput) { a.ExpectedOutputSequence++ }, corestore.ErrRevisionConflict},
			} {
				t.Run(key(v)+"/"+test.name, func(t *testing.T) { arg := base; test.mutate(&arg); finish(t, arg, test.want); assertNormal(t) })
			}
			finished := finish(t, base, nil)
			if finished.Status != "succeeded" || finished.Revision != 3 || finished.OutputSequence != 1 || !finished.ExitCode.Valid || finished.ExitCode.Int32 != 0 || finished.OutputObjectKey == nil || *finished.OutputObjectKey != output.Key || finished.OutputContentBytes != int64(len("example output")) {
				t.Fatalf("terminal output=%+v", finished)
			}
			normal[key(v)] = finished
			assertNormal(t)
			finish(t, base, corestore.ErrRevisionConflict)
			assertNormal(t)
			stream := input(readRun(t, v, "stream_closed"))
			stream.Status = "failed"
			stream.ExitCode = pgtype.Int4{}
			stream.Reason = "stream_closed"
			if row := finish(t, stream, nil); row.Reason != "stream_closed" || row.ExitCode.Valid || row.OutputContentBytes != int64(len("example output")) {
				t.Fatalf("stream teardown=%+v", row)
			}
			if err := repository.WriteRelayAction(t.Context(), v.organization, corestore.Serving, func(w postgres.RelayActionWriter) error {
				if _, err := w.CreateRelaySession(t.Context(), postgres.CreateRelaySessionInput{OrganizationID: v.organization, ProjectID: v.project, WorkspaceID: v.workspace.ID, ID: "relay_sandbox", PrincipalID: "person", Subject: "subject", ConnectionID: "connection_sandbox_" + string(v.project), Channels: []string{"terminal"}, OpenedAt: current}); err != nil {
					return err
				}
				record, err := w.PutTerminalRecording(t.Context(), postgres.PutTerminalRecordingInput{OrganizationID: v.organization, ProjectID: v.project, WorkspaceID: v.workspace.ID, ID: "recording_sandbox", RelaySessionID: "relay_sandbox", StreamID: "stream_sandbox", PrincipalID: "person", Subject: "subject", Isolation: workspacesession.IsolationSandbox, StartedAt: current, FinishedAt: new(current.Add(time.Second)), TerminalCols: 80, TerminalRows: 24, CastObjectKey: output.Key, CastSha256: output.SHA256, CastBytes: output.CompressedSize, CastContentBytes: output.UncompressedSize, UpdatedAt: current.Add(time.Second)})
				if err != nil {
					return err
				}
				if record.Isolation != "sandbox" {
					t.Fatalf("sandbox translated=%+v", record)
				}
				correction := postgres.PutTerminalRecordingInput{OrganizationID: v.organization, ProjectID: v.project, WorkspaceID: v.workspace.ID, ID: record.ID, RelaySessionID: record.RelaySessionID, StreamID: record.StreamID, PrincipalID: record.PrincipalID, Subject: record.Subject, Isolation: record.Isolation, SupportReason: record.SupportReason, StartedAt: record.StartedAt, FinishedAt: new(current.Add(2 * time.Second)), TerminalCols: record.TerminalCols, TerminalRows: record.TerminalRows, CastObjectKey: completeCast.Key, CastSha256: completeCast.SHA256, CastBytes: completeCast.CompressedSize, CastContentBytes: completeCast.UncompressedSize, UpdatedAt: current.Add(2 * time.Second), ExpectedRevision: record.Revision}
				wrong := correction
				wrong.ExpectedRevision = 0
				if _, err := w.PutTerminalRecording(t.Context(), wrong); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("stale corrective revision=%v", err)
				}
				wrong = correction
				wrong.Subject = "other-subject"
				if _, err := w.PutTerminalRecording(t.Context(), wrong); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("corrective subject changed=%v", err)
				}
				corrected, err := w.PutTerminalRecording(t.Context(), correction)
				if err != nil || corrected.Revision != 2 || corrected.CastObjectKey != completeCast.Key || corrected.CastContentBytes != completeCast.UncompressedSize || corrected.FinishedAt == nil || !corrected.FinishedAt.Equal(*correction.FinishedAt) {
					t.Fatalf("longer corrective recording=%+v error=%v", corrected, err)
				}
				shorter := correction
				shorter.ExpectedRevision = corrected.Revision
				shorter.CastObjectKey, shorter.CastSha256, shorter.CastBytes, shorter.CastContentBytes = output.Key, output.SHA256, output.CompressedSize, output.UncompressedSize
				if _, err := w.PutTerminalRecording(t.Context(), shorter); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("shorter correction accepted=%v", err)
				}
				retained, err := w.TerminalRecording(t.Context(), v.project, record.ID)
				if err != nil || !reflect.DeepEqual(retained, corrected) {
					t.Fatalf("correction changed after refusal: got=%+v want=%+v error=%v", retained, corrected, err)
				}
				arg := postgres.AttachRelayRecordingInput{OrganizationID: v.organization, ProjectID: v.project, WorkspaceID: v.workspace.ID, RelaySessionID: "relay_sandbox", RecordingID: record.ID, RecordingArtifact: "original-recording-reference"}
				if err := w.AttachRelayRecording(t.Context(), arg); err != nil {
					return err
				}
				arg.RecordingArtifact = "later-recording-reference"
				if err := w.AttachRelayRecording(t.Context(), arg); err != nil {
					return err
				}
				rows, err := w.RelaySessions(t.Context(), postgres.RelaySessionsInput{OrganizationID: v.organization, ProjectID: v.project, WorkspaceID: v.workspace.ID, ID: "relay", Limit: 2})
				if err != nil {
					return err
				}
				if len(rows) != 1 || rows[0].ID != "relay_sandbox" || rows[0].RecordingArtifact != "original-recording-reference" {
					t.Fatalf("recording reference changed=%+v", rows)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			originalWorkspace := v.workspace
			if err := repository.WriteTenant(t.Context(), v.organization, corestore.Serving, func(w tenantstore.Writer) error {
				owner := w.(postgres.WorkspaceWriter)
				row, err := owner.WorkspaceSession(t.Context(), v.project, v.workspace.ID)
				if err != nil {
					return err
				}
				v.workspace, err = owner.UpdateWorkspaceSession(t.Context(), postgres.WorkspaceUpdate{OrganizationID: v.organization, ProjectID: v.project, ID: row.ID, ExpectedRevision: row.Revision, ExpectedRunnerID: row.RunnerID, ExpectedLeaseID: row.LeaseID, ExpectedFencingToken: row.FencingToken, State: "requested", Reason: workspacesession.ReasonHubRestarted, UpdatedAt: current.Add(time.Second)})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			rebound := input(readRun(t, v, "rebind_exit"))
			rebound.Status = "failed"
			rebound.ExitCode = pgtype.Int4{Int32: 17, Valid: true}
			if row := finish(t, rebound, nil); row.Status != "failed" || row.ExitCode.Int32 != 17 || !row.ExitCode.Valid || row.Reason != "" || row.LeaseID != v.lease.LeaseID || row.FencingToken != v.lease.FencingToken || row.OutputContentBytes != int64(len("example output")) {
				t.Fatalf("cleared binding lost original live exit facts: %+v", row)
			}
			if err := repository.WriteTenant(t.Context(), v.organization, corestore.Serving, func(w tenantstore.Writer) error {
				lease, err := w.Lease(t.Context(), v.lease.LeaseID)
				if err != nil {
					return err
				}
				if lease.ReleasedAt != nil || !lease.ExpiresAt.After(rebound.FinishedAt) {
					t.Fatal("original exit did not retain a live original lease")
				}
				owner := w.(postgres.WorkspaceWriter)
				for _, state := range []string{"starting", "ready"} {
					row, err := owner.WorkspaceSession(t.Context(), v.project, v.workspace.ID)
					if err != nil {
						return err
					}
					v.workspace, err = owner.UpdateWorkspaceSession(t.Context(), postgres.WorkspaceUpdate{OrganizationID: v.organization, ProjectID: v.project, ID: row.ID, ExpectedRevision: row.Revision, ExpectedRunnerID: row.RunnerID, ExpectedLeaseID: row.LeaseID, ExpectedFencingToken: row.FencingToken, State: state, RunnerID: originalWorkspace.RunnerID, MachineID: originalWorkspace.MachineID, LeaseID: originalWorkspace.LeaseID, FencingToken: originalWorkspace.FencingToken, UpdatedAt: current.Add(time.Second)})
					if err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			old := v
			if err := repository.WriteTenant(t.Context(), v.organization, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.Release(t.Context(), pgstoretest.LeaseFence(v.lease), "example"); err != nil {
					return err
				}
				item, err := w.WorkItemByID(t.Context(), v.project, v.lease.IssueID)
				if err != nil {
					return err
				}
				v.lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: v.lease.MachineID, RunnerID: v.runner, SessionID: "rebound_session_" + string(v.project), LeaseID: "rebound_lease_" + string(v.project), TTL: time.Hour})
				if err != nil {
					return err
				}
				owner := w.(postgres.WorkspaceWriter)
				row, err := owner.WorkspaceSession(t.Context(), v.project, v.workspace.ID)
				if err != nil {
					return err
				}
				v.workspace, err = owner.UpdateWorkspaceSession(t.Context(), postgres.WorkspaceUpdate{OrganizationID: v.organization, ProjectID: v.project, ID: row.ID, ExpectedRevision: row.Revision, ExpectedRunnerID: row.RunnerID, ExpectedLeaseID: row.LeaseID, ExpectedFencingToken: row.FencingToken, State: row.State, RunnerID: v.runner, MachineID: row.MachineID, LeaseID: v.lease.LeaseID, FencingToken: v.lease.FencingToken, UpdatedAt: current.Add(time.Second)})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			next := claim(v, "next_generation", current)
			lost := input(readRun(t, v, "lease_lost"))
			finish(t, lost, corestore.ErrRevisionConflict)
			lost.Status = "failed"
			lost.ExitCode = pgtype.Int4{}
			lost.Reason = "lease_lost"
			if row := finish(t, lost, nil); row.Reason != "lease_lost" || row.LeaseID != old.lease.LeaseID || row.FencingToken != old.lease.FencingToken {
				t.Fatalf("old-generation teardown=%+v", row)
			}
			if got := readRun(t, v, "next_generation"); !reflect.DeepEqual(got, next) {
				t.Fatalf("old run changed next generation: got=%+v want=%+v", got, next)
			}
			if err := repository.WriteTenant(t.Context(), v.organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.RemoveRunnerIdentity(t.Context(), v.runner, current.Add(time.Second))
			}); err != nil {
				t.Fatal(err)
			}
			removed := input(next)
			finish(t, removed, corestore.ErrRevisionConflict)
			removed.Status = "failed"
			removed.ExitCode = pgtype.Int4{}
			removed.Reason = "stream_closed"
			finish(t, removed, nil)
			if err := repository.ReadRelayAction(t.Context(), v.organization, corestore.Serving, func(r postgres.RelayActionReader) error {
				_, err := r.WorkspaceRelayOwner(t.Context(), postgres.WorkspaceRelayOwnerInput{OrganizationID: v.organization, ProjectID: v.project, ID: v.workspace.ID, RunnerID: v.runner, MachineID: v.workspace.MachineID, LeaseID: v.lease.LeaseID, FencingToken: v.lease.FencingToken, Now: current})
				return err
			}); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("removed runner admitted=%v", err)
			}
			if err := repository.WriteWorkspace(t.Context(), v.organization, corestore.Serving, func(w postgres.WorkspaceWriter) error {
				row, err := w.WorkspaceSession(t.Context(), v.project, v.workspace.ID)
				if err != nil {
					return err
				}
				for _, state := range []string{"closing", "closed"} {
					row, err = w.UpdateWorkspaceSession(t.Context(), postgres.WorkspaceUpdate{OrganizationID: v.organization, ProjectID: v.project, ID: row.ID, ExpectedRevision: row.Revision, ExpectedRunnerID: row.RunnerID, ExpectedLeaseID: row.LeaseID, ExpectedFencingToken: row.FencingToken, State: state, RunnerID: v.runner, MachineID: row.MachineID, LeaseID: row.LeaseID, FencingToken: row.FencingToken, UpdatedAt: current.Add(2 * time.Second)})
					if err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			closed := input(readRun(t, v, "workspace_closed"))
			closed.Status = "failed"
			closed.ExitCode = pgtype.Int4{}
			closed.Reason = "workspace_closed"
			finish(t, closed, nil)
			if err := repository.WriteTenant(t.Context(), v.organization, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.Release(t.Context(), pgstoretest.LeaseFence(v.lease), "example")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			pgstoretest.RequireSQLState(t, repository.WriteTenant(t.Context(), v.organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.DeleteWorkItem(t.Context(), v.project, tracker.NativeWorkItemID(fmt.Sprintf("wi_%s", string(v.project)[4:])))
			}), "23503")
			deleted := input(readRun(t, v, "retained_dispatch"))
			deleted.Status = "failed"
			deleted.ExitCode = pgtype.Int4{}
			deleted.Reason = "lease_lost"
			finish(t, deleted, nil)
			_, err = admin.ExecContext(t.Context(), `DELETE FROM workspace_sessions WHERE organization_id=$1 AND project_id=$2 AND id=$3`, v.organization, v.project, v.workspace.ID)
			pgstoretest.RequireSQLState(t, err, "23503")
			assertNormal(t)
		}
	})
}
