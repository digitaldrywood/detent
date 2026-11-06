package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestScheduledFailureAttribution(t *testing.T) {
	storage, admin := projectRecordPostgresWithAdmin(t, 3)
	now := time.Now().UTC().Truncate(time.Microsecond)
	details := &memoryRunDetails{}
	service := &Service{database: &database{now: func() time.Time { return now }}, config: Config{TenantRepository: storage, AccessRepository: storage, RunDetails: details, Logger: discardLogger(), now: func() time.Time { return now }}}
	content := eventstore.NewContent(details)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		service.localOrganization = string(org)
		credential := apiCredential{ID: "operator", Hash: apikey.HashToken(string(org) + "_operator"), Scope: apiScopeOperator}
		if err := storage.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: now}); err != nil {
				return err
			}
			return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: credential.ID, Hash: credential.Hash, Name: "Example", Scope: "operator", KeyScope: "write", Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		for i, tc := range []struct {
			state   string
			changes int
			want    string
		}{{"Done", 1, "Rework"}, {"Done", 2, "Done"}, {"Done", 0, "Done"}, {"Rework", 1, "Rework"}, {"In Progress", 1, "In Progress"}, {"Merging", 1, "Merging"}} {
			t.Run(fmt.Sprintf("%s/%d", org, i), func(t *testing.T) {
				project := tracker.ProjectID(fmt.Sprintf("prj_%d", i))
				item := tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", i))
				scope := nativeScope{organization: org, project: project, credential: credential}
				states := nativeFixtureStates()
				states[2].Transitions = append(states[2].Transitions, "Rework")
				states = append(states, tracker.NativeState{Name: "Rework", Dispatchable: true}, tracker.NativeState{Name: "Merging", Dispatchable: true})
				issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: item, Number: 1, Revision: 1, Profile: "native"}, Title: "Example", Body: "Example body", State: tc.state, Actor: tracker.Actor{Kind: "human", PrincipalID: credential.ID}, CreatedAt: now, UpdatedAt: now}
				raw, _ := json.Marshal(issue)
				version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: item, RecordID: string(item), Revision: 1}, raw)
				if err != nil {
					t.Fatal(err)
				}
				repository := "owner/repository"
				head := strings.Repeat("b", 40)
				err = storage.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					record, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: string(project), Slug: strings.ReplaceAll(string(project), "_", "-"), Profile: "native"}, States: states})
					if err != nil {
						return err
					}
					record.Settings.CheckoutRepository = repository
					if _, err := w.UpdateProjectRecord(t.Context(), record); err != nil {
						return err
					}
					if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: credential.ID, ProjectID: project}); err != nil {
						return err
					}
					if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: item, Number: 1, Title: issue.Title, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, WorkflowStateName: &issue.State}); err != nil {
						return err
					}
					_, err = w.CreateCollaborationVersion(t.Context(), version)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				for n := range tc.changes {
					change := tracker.ChangeRequest{OrganizationID: org, ProjectID: project, WorkItemID: item, ID: fmt.Sprintf("change_%d_%d", i, n), Revision: 1, CreatedAt: now, UpdatedAt: now}
					raw, _ := json.Marshal(change)
					row, err := content.PrepareChange(t.Context(), org, raw)
					if err != nil {
						t.Fatal(err)
					}
					row.LandedMergeSHA = new(head)
					row.LandedBaseRef = new("develop")
					if err := storage.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.CreateChange(t.Context(), row); return err }); err != nil {
						t.Fatal(err)
					}
				}
				evidence := gate.ScheduledEvidence{Schema: 1, Repository: repository, OccurrenceKey: strings.Repeat("a", 64), RunID: "123", RunAttempt: "1", JobID: "456", JobName: "Coverage", RunURL: "https://github.com/" + repository + "/actions/runs/123/attempts/1", JobURL: "https://github.com/" + repository + "/actions/jobs/456", HeadSHA: head, Conclusion: "failure"}
				service.config.ImportBackend = scheduledContractImporter(func(_ context.Context, r GitHubImportRequest) (GitHubImportPage, error) {
					if r.Stage == "scheduled" {
						if r.ScheduledBranch != "develop" {
							t.Fatal("wrong branch")
						}
						return GitHubImportPage{Scheduled: &evidence}, nil
					}
					if r.CompareBase != strings.Repeat("a", 40) || r.CompareHead != head {
						t.Fatal("wrong range")
					}
					return GitHubImportPage{Commits: []string{head}}, nil
				})
				body := "Failure evidence" + evidence.Stamp()
				request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "occurrence"}, Body: body, ScheduledFailure: &tracker.ScheduledFailure{PreviousGreenSHA: strings.Repeat("a", 40), Evidence: evidence}}
				for range 2 {
					raw, err := service.createNativeIssueCommand(t.Context(), scope, request)
					if err != nil {
						t.Fatal(err)
					}
					var result struct {
						Attributed bool `json:"attributed"`
					}
					if err := json.Unmarshal(raw, &result); err != nil {
						t.Fatal(err)
					}
					if result.Attributed != (tc.changes == 1) {
						t.Fatalf("attributed=%v", result.Attributed)
					}
				}
				row, version, err := service.readWorkItemRecord(t.Context(), scope, string(item))
				if err != nil {
					t.Fatal(err)
				}
				got, err := service.hydrateWorkItemRecord(t.Context(), row, version)
				if err != nil {
					t.Fatal(err)
				}
				if got.State != tc.want {
					t.Fatalf("state=%s want=%s", got.State, tc.want)
				}
				var comments, transitions int
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM native_comments WHERE organization_id=$1 AND project_id=$2`, org, project).Scan(&comments); err != nil {
					t.Fatal(err)
				}
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND type='workflow.transitioned'`, org, project).Scan(&transitions); err != nil {
					t.Fatal(err)
				}
				wantComments, wantTransitions := 0, 0
				if tc.changes == 1 {
					wantComments = 1
					if tc.state == "Done" {
						wantTransitions = 1
					}
				}
				if comments != wantComments || transitions != wantTransitions {
					t.Fatalf("comments=%d transitions=%d", comments, transitions)
				}
				if comments > 0 {
					var id string
					if err := admin.QueryRowContext(t.Context(), `SELECT id FROM native_comments WHERE organization_id=$1 AND project_id=$2`, org, project).Scan(&id); err != nil {
						t.Fatal(err)
					}
					err := storage.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
						row, err := r.Comment(t.Context(), project, item, id)
						if err != nil {
							return err
						}
						version, err := r.CollaborationVersion(t.Context(), id, row.Revision)
						if err != nil {
							return err
						}
						comment, err := service.hydrateCommentRecord(t.Context(), row, version)
						if err != nil {
							return err
						}
						if comment.Body != body {
							t.Fatal("failure evidence changed")
						}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
				}

			})
		}
	}
}
