package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type boardChangeMutationRepository struct {
	tenantstore.Repository
	mutate      func(tracker.OrganizationID) error
	changeReads int
	reads       int
	failRead    int
	writes      int
	boardError  error
}

func (r *boardChangeMutationRepository) WriteTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	r.writes++
	return r.Repository.WriteTenant(ctx, org, workload, write)
}

func (r *boardChangeMutationRepository) ReadTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	r.reads++
	if r.reads == r.failRead {
		return context.DeadlineExceeded
	}
	return r.Repository.ReadTenant(ctx, org, workload, func(reader tenantstore.Reader) error {
		return read(boardChangeMutationReader{Reader: reader, AuditDispositionReader: reader.(eventstore.AuditDispositionReader), WorkItemRelationshipBatchReader: reader.(tenantstore.WorkItemRelationshipBatchReader), repository: r, organization: org})
	})
}

type boardChangeMutationReader struct {
	tenantstore.Reader
	eventstore.AuditDispositionReader
	tenantstore.WorkItemRelationshipBatchReader
	repository   *boardChangeMutationRepository
	organization tracker.OrganizationID
}

func (r boardChangeMutationReader) BoardItem(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (viewstore.BoardRecord, error) {
	if r.repository.boardError != nil {
		return viewstore.BoardRecord{}, r.repository.boardError
	}
	return r.Reader.BoardItem(ctx, project, item)
}

func (r boardChangeMutationReader) BoardChangeRows(ctx context.Context, project tracker.ProjectID, requests []tenantstore.BoardChangeRequest) ([]tenantstore.BoardChangeRows, error) {
	r.repository.changeReads++
	if r.repository.mutate != nil {
		mutate := r.repository.mutate
		r.repository.mutate = nil
		if err := mutate(r.organization); err != nil {
			return nil, err
		}
	}
	return r.Reader.(tenantstore.BoardChangeRowsReader).BoardChangeRows(ctx, project, requests)
}

func TestBoardProjectionDeletedChangeAndRealErrorsPostgres(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	mutating := &boardChangeMutationRepository{Repository: repository}
	details := &memoryRunDetails{}
	service := &Service{config: Config{TenantRepository: mutating, RunDetails: details}}
	content := eventstore.NewContent(details)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	service.config.now = func() time.Time { return now }
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		policy, err := content.PrepareProjectPolicy(t.Context(), org, hubTestPolicy())
		if err != nil {
			t.Fatal(err)
		}
		policy.ProjectID, policy.ApprovedBy, policy.ApprovedAt = "example", "example", now
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: now}); err != nil {
				return err
			}
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "example", Name: "Example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			return w.ApproveProjectPolicy(t.Context(), policy, "")
		}); err != nil {
			t.Fatal(err)
		}
	}
	var auditCounts [2]int
	for index, test := range []struct {
		name  string
		want  error
		audit bool
		size  int
	}{
		{name: "deleted"},
		{name: "security audit 2", audit: true, size: 2},
		{name: "security audit 50", audit: true, size: 50},
		{name: "missing payload", want: artifact.ErrMissing},
		{name: "database failure", want: context.Canceled},
		{name: "repository entry failure", want: context.DeadlineExceeded},
	} {
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			t.Run(string(org)+"/"+test.name, func(t *testing.T) {
				id := tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", index))
				changeID := fmt.Sprintf("change_%d", index)
				issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "example", WorkItemID: id, Number: index + 1, Revision: 1}, Title: "Example", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: now, UpdatedAt: now}
				raw, err := json.Marshal(issue)
				if err != nil {
					t.Fatal(err)
				}
				collaboration, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "example", WorkItemID: id, RecordID: string(id), Revision: 1}, raw)
				if err != nil {
					t.Fatal(err)
				}
				change := tracker.ChangeRequest{ID: changeID, OrganizationID: org, ProjectID: "example", WorkItemID: id, LinkedIssues: []tracker.NativeWorkItemID{id}, Title: "Example", Revision: 1, CreatedAt: now, UpdatedAt: now}
				raw, err = json.Marshal(change)
				if err != nil {
					t.Fatal(err)
				}
				changeRow, err := content.PrepareChange(t.Context(), org, raw)
				if err != nil {
					t.Fatal(err)
				}
				var versionRow eventstore.Version
				var reviewRow eventstore.Evidence
				if test.audit {
					version := tracker.ChangeVersion{ID: fmt.Sprintf("version_%d", index), ChangeID: changeID, Number: 1, ChangeVersionInput: changeTestInput(), Policy: hubTestPolicy(), CreatedAt: now}
					raw, err := json.Marshal(version)
					if err != nil {
						t.Fatal(err)
					}
					versionRow, err = content.PrepareVersion(t.Context(), org, raw)
					if err != nil {
						t.Fatal(err)
					}
					review := tracker.ChangeReview{ID: fmt.Sprintf("review_%d", index), VersionID: version.ID, Decision: "approved", CreatedAt: now, SecurityAudit: &tracker.SecurityAuditReview{Run: securityaudit.Run{ID: 1, ProjectID: "example", IssueID: string(id)}}}
					raw, err = json.Marshal(review)
					if err != nil {
						t.Fatal(err)
					}
					reviewRow, err = content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: changeID, VersionID: &version.ID, Kind: "review", CreatedAt: now}, raw)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "example", NativeID: id, Number: int64(index + 1), Title: issue.Title, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, WorkflowStateName: &issue.State}); err != nil {
						return err
					}
					if _, err := w.CreateCollaborationVersion(t.Context(), collaboration); err != nil {
						return err
					}
					if _, err := w.CreateChange(t.Context(), changeRow); err != nil {
						return err
					}
					for extra := range max(0, test.size-index-1) {
						paddingID := tracker.NativeWorkItemID(fmt.Sprintf("padding_%d_%d", index, extra))
						if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "example", NativeID: paddingID, Number: int64(100 + index*100 + extra), Title: "Example", WorkflowStateName: &issue.State}); err != nil {
							return err
						}
					}
					if test.audit {
						if _, err := w.CreateVersion(t.Context(), versionRow); err != nil {
							return err
						}
						changeRow.CurrentVersionID = &versionRow.ID
						if _, err := w.UpdateChange(t.Context(), changeRow); err != nil {
							return err
						}
						if _, err := w.AppendEvidence(t.Context(), reviewRow); err != nil {
							return err
						}
					}
					return w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: org, ProjectID: "example", WorkItemID: id, ChangeID: changeID, CreatedAt: now})
				}); err != nil {
					t.Fatal(err)
				}
				mutating.mutate = func(org tracker.OrganizationID) error {
					switch test.name {
					case "deleted":
						if _, err := admin.ExecContext(t.Context(), "DELETE FROM change_issue_links WHERE organization_id=$1 AND change_id=$2", org, changeID); err != nil {
							return err
						}
						_, err := admin.ExecContext(t.Context(), "DELETE FROM change_requests WHERE organization_id=$1 AND id=$2", org, changeID)
						return err
					case "missing payload":
						details.mu.Lock()
						delete(details.data, changeRow.PayloadObjectKey)
						details.mu.Unlock()
					case "database failure":
						return context.Canceled
					}
					return nil
				}
				statements, transactions := 0, 0
				mutating.reads, mutating.failRead = 0, 0
				if test.name == "repository entry failure" {
					mutating.failRead = 2
				}
				ctx := postgres.WithTransactionTiming(t.Context(), func(name string, _ time.Time) {
					if name == "pg_begin" {
						transactions++
					}
				}, func(time.Time, string) { statements++ })
				err = service.refreshBoardRecord(ctx, org, viewstore.BoardCandidate{ProjectID: "example", WorkItemID: id}, now)
				if test.want != nil {
					if !errors.Is(err, test.want) {
						t.Fatalf("refresh error=%v, want %v", err, test.want)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					card, err := r.BoardItem(t.Context(), "example", id)
					if err == nil && (card.OrganizationID != org || !card.Display.Ready || (card.Display.Change != nil) != test.audit) {
						t.Errorf("incorrect refreshed card: %+v", card.Display)
					}
					if err == nil && test.name == "deleted" && card.Display.ChangePrincipalID != "" {
						t.Errorf("deleted Change principal retained: %s", card.Display.ChangePrincipalID)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				var dirty int
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM native_board_dirty WHERE organization_id=$1 AND work_item_id=$2", org, id).Scan(&dirty); err != nil {
					t.Fatal(err)
				}
				if dirty != 0 {
					t.Fatalf("refreshed card remains dirty: %d", dirty)
				}
				if test.name == "deleted" {
					reads := mutating.changeReads
					writes := mutating.writes
					if _, _, err := service.maintainBoardRecord(t.Context(), org, id); err != nil {
						t.Fatal(err)
					}
					if mutating.changeReads != reads || mutating.writes != writes {
						t.Fatal("next tick rebuilt card after deleting its Change")
					}
					mutating.boardError = context.DeadlineExceeded
					if _, _, err := service.maintainBoardRecord(t.Context(), org, id); !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("settled card read failure was masked: %v", err)
					}
					mutating.boardError = nil
					if _, err := admin.ExecContext(t.Context(), "INSERT INTO native_board_dirty(organization_id,project_id,work_item_id) VALUES($1,'example',$2)", org, id); err != nil {
						t.Fatal(err)
					}
					if _, _, err := service.maintainBoardRecord(t.Context(), org, id); err != nil {
						t.Fatal(err)
					}
					writes++
					if mutating.writes != writes {
						t.Fatal("dirty card with unchanged source fences was skipped")
					}
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						if _, err := w.CreateChange(t.Context(), changeRow); err != nil {
							return err
						}
						return w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: org, ProjectID: "example", WorkItemID: id, ChangeID: changeID, CreatedAt: now})
					}); err != nil {
						t.Fatal(err)
					}
					if _, _, err := service.maintainBoardRecord(t.Context(), org, id); err != nil {
						t.Fatal(err)
					}
					if mutating.changeReads != reads+1 || mutating.writes != writes+1 {
						t.Fatal("newly linked Change did not refresh settled card")
					}
				}
				if test.audit {
					var total int
					if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE organization_id=$1 AND project_id='example'", org).Scan(&total); err != nil {
						t.Fatal(err)
					}
					if total != test.size {
						t.Fatalf("fixture has %d issues, want %d", total, test.size)
					}
					counts := [2]int{statements + 2*transactions, statements + transactions}
					if auditCounts == [2]int{} {
						auditCounts = counts
					}
					if counts != auditCounts || counts[0] > 75 || counts[1] > 75 {
						t.Fatalf("size %d statements/exchanges=%v, first fixture=%v (bound 75)", test.size, counts, auditCounts)
					}
					t.Logf("size=%d statements=%d exchanges=%d", test.size, counts[0], counts[1])
					scope := nativeScope{organization: org, project: "example"}
					if _, _, err := service.prepareValidatorAuditDispositions(t.Context(), scope, changeID, securityaudit.Run{ID: 1}); !errors.Is(err, corestore.ErrNotFound) && !errors.Is(err, accessstore.ErrDenied) {
						t.Fatalf("request with missing credentials was accepted: %v", err)
					}
				}
			})
		}
	}
}
