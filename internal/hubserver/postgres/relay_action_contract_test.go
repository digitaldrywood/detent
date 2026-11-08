package postgres_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestRelayActionContractRedeemsOnceAndFencesOrderedReports(t *testing.T) {
	admin, repository, _, now := workspaceContractFixture(t)
	objects := newHistoryObjects()
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
	repository, _ = repositoryFor(t, admin, smallPool())
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
}
