package hubserver

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReportTypedQueryCompatibility(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service := &Service{config: Config{now: func() time.Time { return now }}}
	objects := &memoryRunDetails{}
	content := eventstore.NewContent(objects)
	identities := []struct{ role, requested, resolved, execution, model, stage string }{
		{"code", "requested", "resolved", "", "resolved", "code"},
		{"rework", "requested-only", "", "", "requested-only", "rework"},
		{"", "", "", "", "", ""},
		{"", "", "", "code", "", ""},
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedPostgresHubAdmin(t, repository, service, nativeOrganization{ID: org, Name: "Example", Slug: string(org)})
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "prj_reports", Name: "Example", Slug: "example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "prj_reports", NativeID: "wi_reports", Number: 1, Title: "Example", ActorPrincipalID: new("filer"), CreatedAt: now.Add(-time.Hour)})
			if err != nil {
				return err
			}
			return w.SetWorkItemLabels(t.Context(), "prj_reports", "wi_reports", []string{"label"})
		})
		if err != nil {
			t.Fatal(err)
		}
		for index, identity := range identities {
			id := fmt.Sprintf("runner_%d", index)
			var lease leasestore.Lease
			expires := now.Add(time.Hour)
			err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: id, Name: id, Hash: apikey.HashToken(string(org) + id), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
					return err
				}
				if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: id, Hostname: id, Capacity: 1, Version: "example"}); err != nil {
					return err
				}
				if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: org, ID: id, RunnerID: id, MachineID: tracker.MachineID(id), TokenHash: apikey.HashToken(string(org) + id + "enrollment"), Scope: "projects", Operations: []string{"read"}, CreatedAt: now, ExpiresAt: expires, CreatedBy: "test_hub_admin"}); err != nil {
					return err
				}
				if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: org, RunnerID: id, MachineID: tracker.MachineID(id), TokenID: id, EnrollmentID: id, Scope: "projects", State: "active", Operations: []string{"read"}, CapacityLimit: 1, ReportedCapacity: 1, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
					return err
				}
				item, err := w.WorkItem(t.Context(), "prj_reports", "wi_reports")
				if err != nil {
					return err
				}
				lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: id, SessionID: id, LeaseID: "lease_" + id, TTL: time.Hour})
				if err != nil {
					return err
				}
				return w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: org, RunnerID: id, LeaseID: lease.LeaseID, AssignedAt: now})
			})
			if err != nil {
				t.Fatal(err)
			}
			data := tracker.NativeRunData{AttemptID: fmt.Sprintf("attempt_%d", index), RunID: fmt.Sprintf("run_%d", index), LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), Sequence: 1, RunnerID: id, Outcome: "running", Runtime: &tracker.NativeRuntimeObservation{Identity: agentidentity.Identity{Role: identity.role, RequestedModel: agentidentity.Value{Value: identity.requested}, ResolvedModel: agentidentity.Value{Value: identity.resolved}}}}
			eventType := "run.observed"
			if identity.execution != "" {
				data.Identity = &tracker.NativeExecutionIdentity{Role: identity.execution}
			}
			if index == 0 {
				data.Outcome, eventType = "succeeded", "run.finished"
				data.Runtime.Landing = &tracker.NativeLandingReceipt{RefusalKind: "conflict", ObservedAt: now}
			}
			if index == 1 {
				data.Outcome, eventType = "failed", "run.finished"
				data.TerminalFailure = &tracker.NativeTerminalFailure{Error: "attempt failure"}
			}
			raw, err := json.Marshal(tracker.CollaborationEvent{ID: "event_" + id, OrganizationID: org, ProjectID: "prj_reports", AggregateID: "wi_reports", Type: eventType, SchemaVersion: 1, Actor: tracker.Actor{Kind: "machine", PrincipalID: id}, RecordedAt: now.Add(time.Duration(index) * time.Microsecond), Data: tracker.CollaborationData{Run: &data}})
			if err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := objects.StoreDetail(t.Context(), string(org), payload)
			if err != nil {
				t.Fatal(err)
			}
			event, err := content.PrepareEvent(t.Context(), org, raw)
			if err != nil {
				t.Fatal(err)
			}
			err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				fence := leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}
				if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: data.AttemptID, Fence: fence, RunID: data.RunID, Sequence: 1, Status: data.Outcome, PayloadObjectKey: ref.Key, PayloadReference: ref, StartedAt: now.Add(-time.Minute)}); err != nil {
					return err
				}
				if _, err := w.AppendEvent(t.Context(), event); err != nil {
					return err
				}
				_, err := w.Release(t.Context(), fence, "completed")
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				reader := r.(reportstore.AttemptReader)
				query := reportstore.AttemptQuery{Projects: []tracker.ProjectID{"prj_reports"}, From: now.Add(-time.Hour), To: now.Add(time.Hour), Limit: 10}
				for index, identity := range identities {
					query.Filter = reportstore.Filter{FiledBy: []string{"filer"}, Labels: []string{"label"}, Runners: []string{fmt.Sprintf("runner_%d", index)}, Models: []string{identity.model}, Stages: []string{identity.stage}}
					rows, err := reader.ReadAttemptFacts(t.Context(), query)
					if err != nil {
						return err
					}
					if len(rows) != 1 || rows[0].AttemptID != fmt.Sprintf("attempt_%d", index) || rows[0].Model != identity.model || rows[0].Role != identity.stage {
						t.Fatalf("typed filters for identity %d: %+v", index, rows)
					}
					query.Filter.Runners = []string{"unselected-runner"}
					rows, err = reader.ReadAttemptFacts(t.Context(), query)
					if err != nil {
						return err
					}
					if len(rows) != 0 {
						t.Fatalf("runner filter authorized an unmatched row: %+v", rows)
					}
				}
				events, err := r.Events(t.Context(), "prj_reports", "wi_reports", 0, 10)
				if err != nil {
					return err
				}
				for index, row := range events {
					event, err := content.Event(t.Context(), row)
					if err != nil {
						return err
					}
					got, want := event.Data.Run.Runtime.Identity, identities[index]
					if got.Role != want.role || got.RequestedModel.Value != want.requested || got.ResolvedModel.Value != want.resolved {
						t.Fatalf("typed identity changed: %+v", got)
					}
				}
				if len(events) != 4 {
					t.Fatalf("events=%d", len(events))
				}
				query.Filter = reportstore.Filter{}
				rows, err := reader.ReadAttemptFacts(t.Context(), query)
				if err != nil {
					return err
				}
				if len(rows) != 4 || !rows[0].Failed || !rows[1].Failed || !rows[0].OutcomeAt.Equal(now) || !rows[1].OutcomeAt.Equal(now.Add(time.Microsecond)) || !rows[0].LandingReceipt {
					t.Fatalf("typed failure order, timestamps or landing presence changed: %+v", rows)
				}
				overview, err := r.(reportstore.Reader).ReadOverviewFacts(t.Context(), reportstore.OverviewQuery{Projects: query.Projects, From: now.Add(-time.Hour), Split: now.Add(-30 * time.Minute), To: now.Add(time.Hour), Limit: 10})
				if err != nil {
					return err
				}
				if len(overview) != 1 || overview[0].WorkItemID != "wi_reports" || !overview[0].Worked || overview[0].PreviousWorked {
					t.Fatalf("typed work-item quality: %+v", overview)
				}
				query.From, query.To = now, now.Add(time.Microsecond)
				rows, err = reader.ReadAttemptFacts(t.Context(), query)
				if err != nil {
					return err
				}
				failures := []string{}
				for _, row := range rows {
					if row.Failed {
						failures = append(failures, row.AttemptID)
					}
				}
				if len(failures) != 1 || failures[0] != "attempt_0" {
					t.Fatalf("failure window boundary changed: %v", failures)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
