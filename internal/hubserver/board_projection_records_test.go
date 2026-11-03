package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardProjectionMaintenanceIsBoundedAndTenantScoped(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Date(2026, 11, 1, 22, 0, 0, 0, time.UTC)
	service := &Service{config: Config{TenantRepository: repository, RunDetails: &memoryRunDetails{}, now: func() time.Time { return now }}, localOrganization: "org_first"}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: now}); err != nil {
				return err
			}
			_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "example", Name: "Example", Profile: "native"}, States: nativeFixtureStates()})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for index := range 34 {
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_%02d", index))
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "example", WorkItemID: id, Number: index + 1, Revision: 1}, Title: fmt.Sprintf("Example %d", index), State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: now, UpdatedAt: now}
			raw, err := json.Marshal(issue)
			if err != nil {
				t.Fatal(err)
			}
			version, err := eventstore.NewContent(service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "example", WorkItemID: id, RecordID: string(id), Revision: 1}, raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "example", NativeID: id, Number: int64(index + 1), Title: issue.Title, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, WorkflowStateName: &issue.State}); err != nil {
					return err
				}
				_, err := w.CreateCollaborationVersion(t.Context(), version)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	assertPage := func(org tracker.OrganizationID, count int) int64 {
		t.Helper()
		var sequence int64
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.BoardPage(t.Context(), viewstore.BoardSelection{ProjectID: "example", Limit: 200})
			if err != nil {
				return err
			}
			if len(rows) != count {
				t.Errorf("tenant %s has %d cards, want %d", org, len(rows), count)
			}
			for _, row := range rows {
				if row.OrganizationID != org {
					t.Errorf("cross-tenant card: %+v", row)
				}
			}
			head, err := r.BoardSequence(t.Context(), "example")
			sequence = head.Sequence
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return sequence
	}
	if err := service.maintainBoardRecords(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertPage("org_first", 32)
	assertPage("org_second", 0)
	if err := service.maintainBoardRecords(t.Context()); err != nil {
		t.Fatal(err)
	}
	sequence := assertPage("org_first", 34)
	if err := service.maintainBoardRecords(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := assertPage("org_first", 34); got != sequence {
		t.Fatalf("unchanged refresh advanced sequence %d to %d", sequence, got)
	}
	for _, test := range []struct {
		name   string
		index  int
		cursor string
	}{{"late dirty after cold start", 33, ""}, {"dirty before cursor", 0, "wi_99"}} {
		t.Run(test.name, func(t *testing.T) {
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_%02d", test.index))
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_first", ProjectID: "example", WorkItemID: id, Number: test.index + 1, Revision: 2}, Title: fmt.Sprintf("Example %d", test.index), State: "Done", Terminal: true, Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: now, UpdatedAt: now.Add(time.Second)}
			raw, err := json.Marshal(issue)
			if err != nil {
				t.Fatal(err)
			}
			version, err := eventstore.NewContent(service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: "org_first", ProjectID: "example", WorkItemID: id, RecordID: string(id), Revision: 2}, raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
				row, err := w.WorkItem(t.Context(), "example", id)
				if err != nil {
					return err
				}
				row.WorkflowStateName = new("Done")
				if _, err := w.UpdateWorkItem(t.Context(), row); err != nil {
					return err
				}
				_, err = w.CreateCollaborationVersion(t.Context(), version)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			service.boardRefreshCursor = test.cursor
			if err := service.maintainBoardRecords(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.BoardItem(t.Context(), "example", id)
				if err == nil && (row.SourceRevision != 2 || row.Position.State != "Done") {
					t.Fatalf("dirty card stayed stale after one bounded pass: revision=%d state=%s", row.SourceRevision, row.Position.State)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			assertPage("org_second", 0)
		})
	}
	service.localOrganization, service.boardRefreshCursor = "org_second", ""
	if err := service.maintainBoardRecords(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertPage("org_second", 32)
	if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error { return w.RetireProject(t.Context(), "example", 1, now) }); err != nil {
		t.Fatal(err)
	}
	service.localOrganization, service.boardRefreshCursor = "org_first", ""
	if err := service.maintainBoardRecords(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertPage("org_first", 2)
	if err := service.maintainBoardRecords(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertPage("org_first", 0)
	assertPage("org_second", 32)
}

type boardHistoricalDetailStore struct {
	*memoryRunDetails
	historical      map[string]bool
	historicalReads int
}

func (s *boardHistoricalDetailStore) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if s.historical[ref.Key] {
		s.historicalReads++
	}
	return s.memoryRunDetails.ReadDetail(ctx, organization, ref)
}

func TestBoardProjectionRetainsCoreWhenHistoricalAttemptUnavailable(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	details := &boardHistoricalDetailStore{memoryRunDetails: &memoryRunDetails{}, historical: map[string]bool{}}
	service := &Service{config: Config{TenantRepository: repository, RunDetails: details, now: func() time.Time { return now }}}
	content := eventstore.NewContent(details)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example", CreatedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		for index, test := range []struct {
			name                                     string
			active, missingRow, missingCore, corrupt bool
			want                                     error
		}{
			{name: "historical_missing_row", missingRow: true},
			{name: "historical_missing_object"},
			{name: "active_missing_row", active: true, missingRow: true, want: corestore.ErrNotFound},
			{name: "active_missing_object", active: true, want: artifact.ErrMissing},
			{name: "historical_corrupt_object", corrupt: true},
			{name: "active_corrupt_object", active: true, corrupt: true, want: artifact.ErrIntegrity},
			{name: "missing_current_issue", missingRow: true, missingCore: true, want: artifact.ErrMissing},
		} {
			t.Run(string(organization)+"/"+test.name, func(t *testing.T) {
				project := tracker.ProjectID("project_" + test.name)
				itemID := tracker.NativeWorkItemID("wi_" + test.name)
				issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: organization, ProjectID: project, WorkItemID: itemID, Number: 1, Revision: 1}, Title: "Example", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: now, UpdatedAt: now}
				raw, err := json.Marshal(issue)
				if err != nil {
					t.Fatal(err)
				}
				version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: organization, ProjectID: project, WorkItemID: itemID, RecordID: string(itemID), Revision: 1}, raw)
				if err != nil {
					t.Fatal(err)
				}
				var lease leasestore.Lease
				attemptID := "attempt_" + test.name
				if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: fmt.Sprintf("Example %d", index), Slug: fmt.Sprintf("example-%d", index), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
						return err
					}
					row, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: project, NativeID: itemID, Number: 1, Title: issue.Title, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, WorkflowStateName: &issue.State})
					if err != nil {
						return err
					}
					if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
						return err
					}
					if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: "machine_" + test.name, Hostname: "example", Capacity: 1}); err != nil {
						return err
					}
					lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: row.ID, ExpectedRevision: row.Revision, MachineID: "machine_" + test.name, SessionID: "session_" + test.name, LeaseID: "lease_" + test.name, TTL: time.Hour})
					if err != nil {
						return err
					}
					status := "failed"
					if test.active {
						status = "running"
					}
					if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: attemptID, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: "run_" + test.name, Sequence: 1, Status: status, StartedAt: now, PayloadObjectKey: "example/payload"}); err != nil {
						return err
					}
					if !test.active {
						_, err = w.Release(t.Context(), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, "completed")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if !test.missingRow {
					value := tracker.CollaborationEvent{ID: "event_" + test.name, OrganizationID: organization, ProjectID: project, AggregateType: "issue", AggregateID: itemID, Type: "run.observed", SchemaVersion: 1, Actor: issue.Actor, RecordedAt: now, Data: tracker.CollaborationData{Run: &tracker.NativeRunData{AttemptID: attemptID, RunID: "run_" + test.name, LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), Sequence: 1, MachineID: tracker.MachineID(lease.MachineID)}}}
					raw, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					event, err := content.PrepareEvent(t.Context(), organization, raw)
					if err != nil {
						t.Fatal(err)
					}
					if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), event); return err }); err != nil {
						t.Fatal(err)
					}
					details.historical[event.PayloadObjectKey] = true
					details.mu.Lock()
					if test.corrupt {
						details.data[event.PayloadObjectKey] = []byte(`{}`)
					} else {
						delete(details.data, event.PayloadObjectKey)
					}
					details.mu.Unlock()
				}
				if test.missingCore {
					details.mu.Lock()
					delete(details.data, version.PayloadObjectKey)
					details.mu.Unlock()
				}
				details.historicalReads = 0
				err = service.refreshBoardRecord(t.Context(), organization, viewstore.BoardCandidate{ProjectID: project, WorkItemID: itemID}, lease.RenewedAt.Add(time.Second))
				if !test.active && details.historicalReads != 0 {
					t.Fatal("inactive historical detail was read by core projection")
				}
				if !errors.Is(err, test.want) {
					t.Fatalf("projection error=%v, want %v", err, test.want)
				}
				if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					rows, err := r.BoardPage(t.Context(), viewstore.BoardSelection{ProjectID: project, Limit: 10})
					if err != nil {
						return err
					}
					if test.want != nil {
						if len(rows) != 0 {
							t.Fatal("failed projection published a card")
						}
						return nil
					}
					if len(rows) != 1 || rows[0].WorkItemID != itemID || rows[0].OrganizationID != organization || rows[0].Position.State != "Todo" || !rows[0].Dispatchable || rows[0].Running {
						t.Fatalf("core board projection=%+v", rows)
					}
					card, err := viewstore.NewContent(details).BoardRecord(t.Context(), rows[0])
					if err != nil {
						return err
					}
					if card.Issue.Title != "Example" || card.Attempt == nil || card.Attempt.ID != attemptID || card.Attempt.Count != 1 || card.Attempt.Status != "failed" || card.Attempt.MachineID != lease.MachineID || card.Attempt.Identity != nil {
						t.Fatalf("core card or typed attempt summary lost: %+v", card)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
