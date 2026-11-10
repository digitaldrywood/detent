package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardPageRetainsTypedIssueContract(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	ctx := t.Context()
	at := time.Date(2026, 11, 6, 12, 0, 0, 123000000, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		if err := repository.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error {
			_, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "prj_example", Name: "Example", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "*"}}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for index := 0; index < 4; index++ {
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", index))
			issue := &tracker.NativeIssue{
				NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "prj_example", WorkItemID: id, Revision: 1, Number: index + 1, Profile: "native"},
				Title:           string(org), Body: "Full issue body", State: "Todo", CreatedAt: at, UpdatedAt: at, LastActivityAt: at,
				Labels: []string{}, Assignees: []string{"owner"}, Dependencies: []tracker.NativeWorkItemID{}, Blockers: []tracker.NativeDependency{}, Related: []tracker.NativeDependency{},
				Actor: tracker.Actor{Kind: "human", PrincipalID: "first", Agent: "example"}, Priority: new(2), SchedulingEligible: new(false),
				IssueContract:   &issuecontract.State{ReleaseDelivery: &issuecontract.ReleaseDeliveryApproval{PolicyID: "policy", BodySHA256: "digest"}, Exempt: true, ConfirmedSections: map[string]string{"acceptance": "approved", "scope": "confirmed"}, HumanAction: "approve", ReturnState: "Todo", RecordedAt: at},
				ReportContext:   &tracker.ReportContext{URL: "https://example.com", Browser: "example-browser", OS: "example-os", AppBuild: "build", Viewport: "1200x800", CapturedAt: "capture"},
				ReleaseDelivery: &tracker.ReleaseDelivery{ReleasePickInput: tracker.ReleasePickInput{SourceIssueID: "source", SourceChangeID: "change", SourceMergeSHA: "merge", Repository: "owner/repository", PRNumber: 4, HeadRef: "head", HeadSHA: "headsha", BaseRef: "base", BaseSHA: "basesha", FrozenSHA: "frozen"}, ChangeID: "delivery", VersionID: "version", Actor: tracker.Actor{Kind: "human", PrincipalID: "publisher"}, AssociatedAt: at},
				Verification:    &tracker.NativeVerification{State: "verified", VerifiedBy: &tracker.Actor{Kind: "human", PrincipalID: "verifier"}, VerifiedAt: &at},
				ListChange:      &tracker.NativeListChange{LandingPending: true, Branch: "feature", HeadSHA: "sha"}, PullRequest: &tracker.NativePullRequest{Number: 7, URL: "https://example.com/pull/7"},
				Provenance:         &tracker.Provenance{Provider: "external", ExternalID: "ext", AuthorID: "author", AuthorDisplayName: "Author", CreatedAt: at, UpdatedAt: at, ObservedAt: at},
				Dependents:         []tracker.NativeDependency{{ID: "dependent", ProjectID: "prj_example", Number: 9, Title: "Dependent", State: "Done", Terminal: true, ClosedAt: &at, StartedAt: &at}},
				ExternalReferences: []tracker.ExternalReference{{Provider: "github", Kind: "issue", ID: "123", Number: 123, Repository: "owner/repository", URL: "https://example.com/123"}},
				LinkedSource:       &tracker.LinkedIssueSource{URL: "https://example.com/123", Status: "imported", Snapshot: &tracker.GitHubIssueSnapshot{URL: "https://example.com/123", Title: "Source", Body: "source body", Comments: []tracker.GitHubIssueComment{{Body: "source comment", Provenance: tracker.Provenance{Provider: "github", ExternalID: "comment", CreatedAt: at}}}}},
			}
			if index == 3 {
				issue.State = "*"
			}
			if index == 1 {
				issue = &tracker.NativeIssue{NativeReference: issue.NativeReference, Labels: nil, Assignees: []string{}, IssueContract: &issuecontract.State{ConfirmedSections: map[string]string{}}}
			}
			if index == 2 {
				issue = nil
			}
			ref, err := newHistoryObjects().StoreDetail(ctx, string(org), []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			reporter := &viewstore.BoardReporterRelations{ReporterStatus: "new", Dependencies: []tracker.NativeWorkItemID{}, Blockers: []tracker.NativeDependency{}, Related: []tracker.NativeDependency{}, DependencyReady: true, BlockedReason: tracker.DependencyReason{Text: "private relationship"}}
			if index == 0 {
				reporter.Dependents = []tracker.NativeDependency{{ID: "dependent", ClosedAt: &at}}
			}
			if index == 2 {
				reporter = nil
			}
			record := viewstore.BoardRecord{ReporterRelations: reporter, OrganizationID: org, ProjectID: "prj_example", WorkItemID: id, SourceRevision: 1, Position: viewstore.BoardPosition{State: func() string {
				if index == 3 {
					return "*"
				}
				return "Todo"
			}(), Identifier: fmt.Sprintf("example#%d", index+1)}, Payload: ref, PageIssue: issue, ListIssue: issue, ProjectedAt: at}
			if err := repository.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error {
				_, err := w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: org, ProjectID: record.ProjectID, NativeID: id, Number: int64(index + 1), Title: "Example", WorkflowStateName: new(record.Position.State)})
				if err != nil {
					return err
				}
				return w.ReplaceBoardItem(ctx, viewstore.BoardUpdate{ProjectID: record.ProjectID, WorkItemID: id, ExpectedRevision: 1, Current: &record})
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
				got, err := r.BoardItem(ctx, record.ProjectID, id)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(got.PageIssue, issue) {
					t.Errorf("tenant=%s variant=%d typed issue mismatch: got %#v want %#v", org, index, got.PageIssue, issue)
				}
				if !reflect.DeepEqual(got.ReporterRelations, reporter) {
					t.Errorf("tenant=%s variant=%d reporter relations mismatch: got %#v want %#v", org, index, got.ReporterRelations, reporter)
				}
				if !got.ProjectedAt.Equal(at) {
					t.Errorf("projection checkpoint=%v, want %v", got.ProjectedAt, at)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error {
				before, err := w.BoardSequence(ctx, record.ProjectID)
				if err != nil {
					return err
				}
				replay, err := w.BoardItem(ctx, record.ProjectID, id)
				if err != nil {
					return err
				}
				if replay.ActorDisplayName == nil {
					t.Fatal("board read did not load derived actor name")
				}
				actorDisplayName := *replay.ActorDisplayName
				wantActorName := ""
				if index == 0 || index == 3 {
					wantActorName = "first@example.com"
				}
				if actorDisplayName != wantActorName {
					t.Fatalf("tenant=%s variant=%d actor name=%q, want %q", org, index, actorDisplayName, wantActorName)
				}
				if index%2 == 0 {
					replay.ActorDisplayName = nil
				} else {
					replay.ActorDisplayName = new("Projection-local alias")
				}
				summary := w.(interface {
					BoardSummaryModel(context.Context, viewstore.BoardSelection) ([]viewstore.BoardSummaryRecord, error)
				})
				var summaries []viewstore.BoardSummaryRecord
				if index < 2 {
					summaries, err = summary.BoardSummaryModel(ctx, viewstore.BoardSelection{ProjectID: record.ProjectID})
					if err != nil {
						return err
					}
				}
				replay.ProjectedAt = at.Add(time.Minute)
				if replay.PageIssue != nil {
					replay.PageIssue.CreatedAt = replay.PageIssue.CreatedAt.Add(123 * time.Nanosecond).In(time.FixedZone("example", 3600))
				}
				if replay.ReporterRelations != nil && len(replay.ReporterRelations.Dependents) > 0 {
					value := replay.ReporterRelations.Dependents[0].ClosedAt.Add(123 * time.Nanosecond).In(time.FixedZone("example", 3600))
					replay.ReporterRelations.Dependents[0].ClosedAt = &value
				}
				if err := w.ReplaceBoardItem(ctx, viewstore.BoardUpdate{ProjectID: record.ProjectID, WorkItemID: id, ExpectedRevision: 1, Current: &replay}); err != nil {
					return err
				}
				after, err := w.BoardSequence(ctx, record.ProjectID)
				if err != nil {
					return err
				}
				if after != before {
					t.Errorf("tenant=%s variant=%d replay advanced sequence: before=%+v after=%+v", org, index, before, after)
				}
				if index < 2 {
					updated, err := summary.BoardSummaryModel(ctx, viewstore.BoardSelection{ProjectID: record.ProjectID})
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(updated, summaries) {
						t.Errorf("tenant=%s variant=%d replay changed summaries: before=%+v after=%+v", org, index, summaries, updated)
					}
				}
				got, err := w.BoardItem(ctx, record.ProjectID, id)
				if err == nil && (got.ActorDisplayName == nil || *got.ActorDisplayName != actorDisplayName) {
					t.Errorf("replay changed actor name hydration: got=%v want=%q", got.ActorDisplayName, actorDisplayName)
				}
				if err == nil && !got.ProjectedAt.Equal(replay.ProjectedAt) {
					t.Errorf("replay did not refresh projection checkpoint: got=%v want=%v", got.ProjectedAt, replay.ProjectedAt)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}

		}
		if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.(interface {
				BoardSummaryModel(context.Context, viewstore.BoardSelection) ([]viewstore.BoardSummaryRecord, error)
			}).BoardSummaryModel(ctx, viewstore.BoardSelection{ProjectID: "prj_example"})
			if !errors.Is(err, searchstore.ErrIndexPending) || len(rows) != 0 {
				t.Errorf("incomplete typed fixture exposed counts=%+v error=%v", rows, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error {
			if err := w.DeleteWorkItem(ctx, "prj_example", "wi_2"); err != nil {
				return err
			}
			return w.ReplaceBoardItem(ctx, viewstore.BoardUpdate{ProjectID: "prj_example", WorkItemID: "wi_2"})
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.(interface {
				BoardSummaryModel(context.Context, viewstore.BoardSelection) ([]viewstore.BoardSummaryRecord, error)
			}).BoardSummaryModel(ctx, viewstore.BoardSelection{ProjectID: "prj_example", Archived: new(false)})
			if err != nil {
				return err
			}
			lane, total := int64(-1), int64(-1)
			for _, row := range rows {
				if row.Kind == "lane" && row.State == "*" {
					lane = row.Total
				}
				if row.Kind == "total" {
					total = row.Total
				}
			}
			if lane != 1 || total != 3 {
				t.Errorf("configured star lane collided with summary: lane=%d total=%d", lane, total)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}

	}
}
