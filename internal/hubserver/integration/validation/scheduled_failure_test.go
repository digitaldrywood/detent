package validation_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type attributionImporter func(context.Context, hubserver.GitHubImportRequest) (hubserver.GitHubImportPage, error)

func (f attributionImporter) FetchImportPage(ctx context.Context, request hubserver.GitHubImportRequest) (hubserver.GitHubImportPage, error) {
	return f(ctx, request)
}

func TestScheduledFailureAttribution(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Microsecond)
	repository := "owner/repository"
	head := strings.Repeat("b", 40)
	evidence := gate.ScheduledEvidence{Schema: 1, Repository: repository, OccurrenceKey: strings.Repeat("a", 64), RunID: "123", RunAttempt: "1", JobID: "456", JobName: "Coverage", RunURL: "https://github.com/" + repository + "/actions/runs/123/attempts/1", JobURL: "https://github.com/" + repository + "/actions/jobs/456", HeadSHA: head, Conclusion: "failure"}
	importer := attributionImporter(func(_ context.Context, request hubserver.GitHubImportRequest) (hubserver.GitHubImportPage, error) {
		switch request.Stage {
		case "scheduled":
			if request.ScheduledBranch != "develop" {
				t.Fatal("wrong branch")
			}
			return hubserver.GitHubImportPage{Scheduled: &evidence}, nil
		case "scheduled-range":
			if request.CompareBase != strings.Repeat("a", 40) || request.CompareHead != head {
				t.Fatal("wrong range")
			}
			return hubserver.GitHubImportPage{Commits: []string{head}}, nil
		default:
			return hubserver.GitHubImportPage{}, fmt.Errorf("unexpected import stage %q", request.Stage)
		}
	})
	hub := hubfixture.Open(t, hubserver.Config{Clock: func() time.Time { return now }, ImportBackend: importer})
	storage := hub.Repository
	content := eventstore.NewContent(hub.Config.RunDetails)
	type attributionCase struct {
		state   string
		changes int
		want    string
	}
	for _, org := range []tracker.OrganizationID{hub.Primary, hubfixture.Secondary} {
		cases := []attributionCase{{"Done", 1, "Rework"}, {"Done", 2, "Done"}, {"Done", 0, "Done"}, {"Rework", 1, "Rework"}, {"In Progress", 1, "In Progress"}, {"Merging", 1, "Merging"}}
		if org != hub.Primary {
			cases = []attributionCase{{"Done", 0, "Done"}}
		}
		for i, tc := range cases {
			t.Run(fmt.Sprintf("%s/%d", org, i), func(t *testing.T) {
				project := tracker.ProjectID(fmt.Sprintf("prj_%d", i))
				item := tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", i))
				states := hubtest.States()
				states[2].Transitions = append(states[2].Transitions, "Rework")
				states = append(states, tracker.NativeState{Name: "Rework", Dispatchable: true}, tracker.NativeState{Name: "Merging", Dispatchable: true})
				issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: item, Number: 1, Revision: 1, Profile: "native"}, Title: "Example", Body: "Example body", State: tc.state, Actor: tracker.Actor{Kind: "human", PrincipalID: hubfixture.AdminCredentialID}, CreatedAt: now, UpdatedAt: now}
				raw, _ := json.Marshal(issue)
				version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: item, RecordID: string(item), Revision: 1}, raw)
				if err != nil {
					t.Fatal(err)
				}
				err = storage.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					record, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: string(project), Slug: strings.ReplaceAll(string(project), "_", "-"), Profile: "native"}, States: states})
					if err != nil {
						return err
					}
					record.Settings.CheckoutRepository = repository
					if _, err := w.UpdateProjectRecord(t.Context(), record); err != nil {
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
				actor := hubtest.Credential(t, storage, org, hubtest.CredentialOptions{KeyScope: "write", Projects: []tracker.ProjectID{project}, Now: now})
				base := hubfixture.ProjectBase(org, project) + "/work-items"
				body := "Failure evidence" + evidence.Stamp()
				request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "occurrence"}, Body: body, ScheduledFailure: &tracker.ScheduledFailure{PreviousGreenSHA: strings.Repeat("a", 40), Evidence: evidence}}
				for range 2 {
					response := hub.Request(t, http.MethodPost, base, actor.Token, request)
					hubfixture.RequireStatus(t, response, http.StatusOK)
					var result struct {
						Attributed bool `json:"attributed"`
					}
					hubfixture.Decode(t, response, &result)
					if result.Attributed != (tc.changes == 1) {
						t.Fatalf("attributed=%v", result.Attributed)
					}
				}
				itemPath := base + "/" + string(item)
				response := hub.Request(t, http.MethodGet, itemPath, actor.Token, nil)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var got tracker.NativeIssue
				hubfixture.Decode(t, response, &got)
				if got.State != tc.want {
					t.Fatalf("state=%s want=%s", got.State, tc.want)
				}
				response = hub.Request(t, http.MethodGet, itemPath+"/comments", actor.Token, nil)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var comments tracker.Page[tracker.NativeComment]
				hubfixture.Decode(t, response, &comments)
				var wantComments []string
				if tc.changes == 1 {
					wantComments = []string{body}
				}
				var commentBodies []string
				for _, comment := range comments.Items {
					commentBodies = append(commentBodies, comment.Body)
				}
				if !slices.Equal(commentBodies, wantComments) {
					t.Fatalf("comments=%q want=%q", commentBodies, wantComments)
				}
				response = hub.Request(t, http.MethodGet, itemPath+"/history", actor.Token, nil)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var history tracker.Page[tracker.CollaborationEvent]
				hubfixture.Decode(t, response, &history)
				var transitions []string
				for _, event := range history.Items {
					if event.Type == "workflow.transitioned" {
						transitions = append(transitions, event.Data.FromState+" to "+event.Data.ToState)
					}
				}
				var wantTransitions []string
				if tc.state != tc.want {
					wantTransitions = []string{tc.state + " to " + tc.want}
				}
				if !slices.Equal(transitions, wantTransitions) {
					t.Fatalf("transitions=%q want=%q", transitions, wantTransitions)
				}
			})
		}
	}
}
