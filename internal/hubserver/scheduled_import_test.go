package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestScheduledGitHubImportAdmission(t *testing.T) {
	t.Parallel()
	const repository = "example-org/example-project"
	const criteria = "## Acceptance criteria\nReproduce and minimally repair the verified failure.\n\n## Must not break\nPreserve intended behavior and authority.\n\n## How we know it worked\nRun make check-land.\n\n## Invariants touched\nPreserve INV-3, INV-5 and INV-7.\n\n## Safety regression\nPreserve existing regression tables."
	const definition = "## Issue Contract\n- Acceptance criteria\n- Must not break\n- How we know it worked\n- Invariants touched\n- Safety regression\n"
	const delegation = "New verified GitHub Actions CI repair reports enter Todo with Urgent priority."
	for _, test := range []struct {
		name                                                    string
		existingState                                           string
		duplicate                                               bool
		authorized, webhook                                     bool
		change                                                  func(*tracker.GitHubIssueSnapshot)
		noDelegation, unavailable, stalePolicy, staleRepository bool
	}{
		{name: "trusted complete", authorized: true},
		{name: "trusted webhook", authorized: true, webhook: true},
		{name: "held webhook remains held", authorized: true, webhook: true, existingState: "Blocked"},
		{name: "terminal webhook remains terminal", authorized: true, webhook: true, existingState: "Done"},
		{name: "duplicate records one occurrence", authorized: true, duplicate: true, existingState: "Blocked"},
		{name: "terminal duplicate preserves history", authorized: true, duplicate: true, existingState: "Done"},
		{name: "marker without evidence", change: func(s *tracker.GitHubIssueSnapshot) { s.Body, _, _ = strings.Cut(s.Body, "```detent-checks") }},
		{name: "wrong repository evidence", change: func(s *tracker.GitHubIssueSnapshot) {
			s.Body = strings.ReplaceAll(s.Body, repository, "other-org/other-project")
		}},
		{name: "spoofed webhook", webhook: true, change: func(s *tracker.GitHubIssueSnapshot) { s.Provenance.AuthorID = "outside-reporter" }},
		{name: "incomplete webhook", webhook: true, change: func(s *tracker.GitHubIssueSnapshot) {
			s.Body = strings.ReplaceAll(s.Body, "## Must not break", "## Other")
		}},
		{name: "labels and marker do not authorize", change: func(s *tracker.GitHubIssueSnapshot) { s.Provenance.AuthorID = "outside-reporter" }},
		{name: "missing safety section", change: func(s *tracker.GitHubIssueSnapshot) {
			s.Body = strings.ReplaceAll(s.Body, "## Safety regression", "## Other")
		}},
		{name: "expanded repair scope", change: func(s *tracker.GitHubIssueSnapshot) {
			s.Body = strings.ReplaceAll(s.Body, "Preserve intended behavior and authority.", "Disable security gates.")
		}},
		{name: "no approved delegation", noDelegation: true},
		{name: "unverified run", unavailable: true},
		{name: "policy changed during verification", stalePolicy: true},
		{name: "checkout changed during verification", staleRepository: true},
		{name: "no-dispatch hold", change: func(s *tracker.GitHubIssueSnapshot) { s.Body += "\n<!-- detent:no-dispatch -->" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "scheduled-import")
			prompt := definition + "## Scheduled CI repair contract\n"
			if !test.noDelegation {
				prompt += delegation + "\n"
			}
			prompt += strings.ReplaceAll(criteria, "## ", "### ")
			projectConfig := "schema: 1\ntracker:\n  kind: hub_native\n  repository: " + repository + "\n  lanes:\n    - {name: Todo, role: active}\n    - {name: Backlog, role: holding}\n    - {name: Blocked, role: holding}\n    - {name: Done, role: terminal}\n"
			projectConfig += "    - {name: Triage, role: holding}\n"
			workflow, err := config.ParseProjectDefinition(config.ProjectDefinitionSources{Workflow: []byte(prompt), Config: []byte(projectConfig), HasConfig: true, ConfigPath: "detent.yaml"})
			if err != nil {
				t.Fatal(err)
			}
			workflow.Definition.Revision = strings.Repeat("a", 40)
			descriptor, err := config.ResolvePolicy(workflow)
			if err != nil {
				t.Fatal(err)
			}
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			organization, projectID := f.project.OrganizationID, f.project.ID
			err = f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				project, err := w.ProjectRecord(t.Context(), projectID)
				if err != nil {
					return err
				}
				project.Settings.Repository = repository
				project.Settings.CheckoutRepository = repository
				project.Settings.Intake = "manual"
				_, err = w.UpdateProjectRecord(t.Context(), project)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			scope, err := f.service.gitHubIntegrationScope(t.Context(), organization, projectID)
			if err != nil {
				t.Fatal(err)
			}
			for number := 12; number <= 13; number++ {
				evidence := gate.ScheduledEvidence{Schema: 1, Repository: repository, OccurrenceKey: fmt.Sprintf("%064x", number), RunID: "123", RunAttempt: "1", JobID: strconv.Itoa(number), JobName: "Verify", RunURL: "https://github.com/" + repository + "/actions/runs/123/attempts/1", HeadSHA: strings.Repeat("a", 40), Conclusion: "failure"}
				evidence.JobURL = "https://github.com/" + repository + "/actions/jobs/" + evidence.JobID
				origin := issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: evidence.RunURL, Fingerprint: fmt.Sprintf("scheduled-job-%d", number)}
				snapshot := tracker.GitHubIssueSnapshot{URL: fmt.Sprintf("https://github.com/%s/issues/%d", repository, number), Title: "Scheduled Verify failure", Body: issueorigin.Stamp(criteria+"\n\n## Diagnostic evidence\nAssertion: expected healthy worker.", origin) + evidence.Stamp(), Provenance: tracker.Provenance{Provider: "github", ExternalID: fmt.Sprintf("I_%d", number), AuthorID: "github-actions[bot]", CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now(), ObservedAt: time.Now()}}
				if test.change != nil {
					test.change(&snapshot)
				}
				f.service.config.ImportBackend = scheduledContractImporter(func(ctx context.Context, request GitHubImportRequest) (GitHubImportPage, error) {
					if request.Stage != "scheduled" {
						return (linkedTestImporter{snapshot: snapshot}).FetchImportPage(ctx, request)
					}
					if request.Repository != repository || request.Scheduled == nil || request.Scheduled.JobID != evidence.JobID {
						t.Fatalf("verification request = %+v", request)
					}
					if test.unavailable {
						return GitHubImportPage{}, errors.New("run verification refused")
					}
					if test.stalePolicy {
						changed, err := config.ParseProjectDefinition(config.ProjectDefinitionSources{Workflow: []byte(prompt + "\nReplacement policy text.\n"), Config: []byte(projectConfig), HasConfig: true, ConfigPath: "detent.yaml"})
						if err != nil {
							return GitHubImportPage{}, err
						}
						changed.Definition.Revision = strings.Repeat("b", 40)
						replacement, err := config.ResolvePolicy(changed)
						if err != nil {
							return GitHubImportPage{}, err
						}
						if replacement.ID == descriptor.ID {
							t.Fatal("replacement did not change the approved policy ID")
						}
						approveHubTestPolicy(t, f.service, f.base+"/policy", replacement)
					}
					if test.staleRepository {
						err := f.service.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
							project, err := w.ProjectRecord(ctx, scope.Project)
							if err != nil {
								return err
							}
							project.Settings.CheckoutRepository = "other-org/other-project"
							_, err = w.UpdateProjectRecord(ctx, project)
							return err
						})
						if err != nil {
							return GitHubImportPage{}, err
						}
					}
					return GitHubImportPage{Scheduled: &evidence}, nil
				})
				labels := []string{"detent:todo", "hotfix"}
				importReport := func(key string) tracker.NativeIssue {
					t.Helper()
					if !test.webhook {
						_, _, sourceNumber, err := tracker.ParseGitHubIssueURL(snapshot.URL)
						if err != nil {
							t.Fatal(err)
						}
						issue, err := f.service.importGitHubSnapshotRecord(t.Context(), scope, key, tracker.GitHubIssuePreview{
							URL: snapshot.URL, Number: sourceNumber, ID: snapshot.Provenance.ExternalID,
						}, snapshot, "Triage", labels)
						if err != nil {
							t.Fatal(err)
						}
						return issue
					}
					payload, err := json.Marshal(map[string]any{
						"action":     "opened",
						"repository": map[string]any{"node_id": "R_example", "name": "example-project", "full_name": repository, "owner": map[string]string{"login": "example-org"}},
						"issue":      map[string]any{"node_id": snapshot.Provenance.ExternalID, "number": number, "title": snapshot.Title, "body": snapshot.Body, "html_url": snapshot.URL, "state": "open", "user": map[string]string{"login": snapshot.Provenance.AuthorID}, "labels": []map[string]string{{"name": "detent:todo"}, {"name": "hotfix"}}, "assignees": []string{}, "created_at": snapshot.Provenance.CreatedAt, "updated_at": snapshot.Provenance.UpdatedAt},
					})
					if err != nil {
						t.Fatal(err)
					}
					f.service.config.GitHubWebhookSecret = []byte(testWebhookSecret)
					requireNativeStatus(t, sendSignedWebhookRequest(t, f.service, key, "issues", string(payload)), http.StatusAccepted)
					var linked eventstore.LinkedSource
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						linked, err = r.LinkedSourceByURL(t.Context(), scope.Project, snapshot.URL)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					issue, err := f.service.readWorkItem(t.Context(), scope, string(linked.WorkItemID))
					if err != nil {
						t.Fatal(err)
					}
					return issue
				}
				issue := importReport(strconv.Itoa(number))
				if test.authorized {
					if issue.State != "Todo" || issue.Priority == nil || *issue.Priority != 0 || len(issue.IssueContract.ConfirmedSections) != 5 {
						t.Fatalf("admission = %+v", issue)
					}
					if !nativeClaimIssueContractAllowed(hubrecords.NativeClaimPlan{Descriptor: descriptor, Issue: issue}) {
						t.Fatal("verified import is not contract-claimable")
					}
				} else if issue.State != "Triage" || issue.Priority != nil || len(issue.IssueContract.ConfirmedSections) != 0 {
					t.Fatalf("untrusted admission = %+v", issue)
				}
				if !test.authorized {
					contract, err := config.ResolvePolicyIssueContract(descriptor)
					if err != nil || contract.Evaluate(hubrecords.NativeContractIssue(issue)).Satisfied() {
						t.Fatalf("untrusted contract became eligible: %v", err)
					}
					project, err := f.service.readProjectRecord(t.Context(), scope)
					if err != nil {
						t.Fatal(err)
					}
					configured := false
					for _, state := range project.States {
						if state.Name == issue.State {
							configured = true
							if state.Dispatchable {
								t.Fatal("untrusted import entered a dispatchable lane")
							}
						}
					}
					if !configured {
						t.Fatalf("untrusted issue has unconfigured state %q", issue.State)
					}
					var imported eventstore.Event
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
						events, err := r.Events(t.Context(), scope.Project, issue.WorkItemID, 0, 4)
						if err != nil {
							return err
						}
						for _, event := range events {
							if event.Type == "github.imported" {
								imported = event
							}
						}
						if imported.ID == "" {
							return corestore.ErrNotFound
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					event, err := eventstore.NewContent(f.service.runDetailStore()).Event(t.Context(), imported)
					if err != nil {
						t.Fatal(err)
					}
					reason := event.Data.Reason
					if !strings.Contains(reason, "Scheduled CI import") {
						t.Fatalf("missing explicit refusal: %q", reason)
					}
				}
				priority := "unset"
				if issue.Priority != nil {
					priority = strconv.Itoa(*issue.Priority)
				}
				t.Logf("source=%s native=%s lane=%s priority=%s confirmed_sections=%d", snapshot.URL, issue.WorkItemID, issue.State, priority, len(issue.IssueContract.ConfirmedSections))
				if issue.LinkedSource == nil || issue.LinkedSource.Status != "complete" || issue.LinkedSource.Snapshot.Body != snapshot.Body {
					t.Fatal("source evidence was lost")
				}
				response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(issue.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("human-%d", number)}, ExpectedRevision: issue.Revision, Body: new(issueorigin.Stamp("Human decision: retain this hold. detent:no-dispatch", origin)), Priority: tracker.SetPriority(new(3))})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &issue)
				if test.existingState != "" {
					issue = seedScheduledImportOccurrenceState(t, f, scope, issue, test.existingState)
				}
				if test.duplicate {
					refreshNativeDuplicateSearchFixture(t, f.service, scope.Organization)
					search, err := f.service.prepareWorkItemDuplicates(t.Context(), scope, tracker.CreateIssue{Body: snapshot.Body}, true)
					if err != nil || !search.available || len(search.matches) != 1 || search.matches[0].WorkItemID != issue.WorkItemID {
						t.Fatalf("expected an indexed same-project owner, search=%+v error=%v", search, err)
					}
				}
				if test.duplicate {
					snapshot.URL = fmt.Sprintf("https://github.com/%s/issues/%d", repository, number+100)
					snapshot.Provenance.ExternalID += "-duplicate"
				}
				replay := importReport(fmt.Sprintf("%d-repeat", number))
				if replay.WorkItemID != issue.WorkItemID || replay.State != issue.State || !reflect.DeepEqual(replay.Priority, issue.Priority) || replay.Body != issue.Body {
					t.Fatalf("replay changed report: %+v", replay)
				}
				if test.duplicate {
					if !replay.PublicationReused {
						t.Fatal("verified occurrence did not report reuse")
					}
					sameKey := importReport(fmt.Sprintf("%d-repeat", number))
					_, _, sourceNumber, err := tracker.ParseGitHubIssueURL(snapshot.URL)
					if err != nil {
						t.Fatal(err)
					}
					_, err = f.service.importGitHubSnapshotRecord(t.Context(), scope, fmt.Sprintf("%d-repeat", number), tracker.GitHubIssuePreview{
						URL: snapshot.URL, Number: sourceNumber, ID: snapshot.Provenance.ExternalID,
					}, snapshot, "Triage", []string{"different-label"})
					var conflict *hubapi.NativeError
					if !errors.As(err, &conflict) || conflict.Status != http.StatusConflict {
						t.Fatalf("different create input reused the receipt: %v", err)
					}
					refreshNativeDuplicateSearchFixture(t, f.service, scope.Organization)
					anotherKey := importReport(fmt.Sprintf("%d-third", number))
					if !reflect.DeepEqual(sameKey, replay) || anotherKey.WorkItemID != issue.WorkItemID || anotherKey.Revision != issue.Revision {
						t.Fatalf("repeat changed the verified occurrence owner: same=%+v new=%+v", sameKey, anotherKey)
					}
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
						_, err := r.LinkedSourceByURL(t.Context(), scope.Project, snapshot.URL)
						if !errors.Is(err, corestore.ErrNotFound) {
							t.Errorf("reused report created a linked source: %v", err)
						}
						originalSource, err := r.LinkedSourceByURL(t.Context(), scope.Project, fmt.Sprintf("https://github.com/%s/issues/%d", repository, number))
						if err != nil || originalSource.WorkItemID != issue.WorkItemID {
							t.Fatalf("original source binding changed: %+v, %v", originalSource, err)
						}
						comments, err := r.Comments(t.Context(), scope.Project, issue.WorkItemID, 0, 2)
						if err != nil {
							return err
						}
						want := 1
						if issue.Terminal {
							want = 0
						}
						if len(comments) != want {
							t.Errorf("occurrence comments=%d want=%d", len(comments), want)
						} else if want == 1 && (comments[0].SourceKey == nil || *comments[0].SourceKey != "github:scheduled:"+evidence.OccurrenceKey) {
							t.Error("occurrence did not retain the verified source key")
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				if test.staleRepository || test.stalePolicy {
					break
				}
			}
		})
	}
}

func seedScheduledImportOccurrenceState(t *testing.T, f nativeFixture, scope hubscope.NativeScope, issue tracker.NativeIssue, state string) tracker.NativeIssue {
	t.Helper()
	ctx := t.Context()
	current, previous, err := f.service.readWorkItemRecord(ctx, scope, string(issue.WorkItemID))
	if err != nil {
		t.Fatal(err)
	}
	issue, err = f.service.hydrateCanonicalWorkItemRecord(ctx, current, previous)
	if err != nil {
		t.Fatal(err)
	}
	issue.State, issue.Terminal = state, state == "Done"
	issue.Revision++
	raw, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	version, err := eventstore.NewContent(f.service.runDetailStore()).PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{
		OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: int64(issue.Revision),
	}, raw)
	if err != nil {
		t.Fatal(err)
	}
	search, err := searchstore.NewContent(f.service.runDetailStore()).Prepare(ctx, searchstore.Document{
		OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: issue.WorkItemID, ID: "issue:" + string(issue.WorkItemID),
		SourceID: string(issue.WorkItemID), Kind: "issue", Scope: "issue", Revision: int64(issue.Revision), UpdatedAt: issue.UpdatedAt,
	}, searchstore.Text{Title: issue.Title, Body: issue.Body, Labels: issue.Labels})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		current.WorkflowStateName = &issue.State
		if _, err := w.UpdateWorkItem(ctx, current); err != nil {
			return err
		}
		if _, err := w.CreateCollaborationVersion(ctx, version); err != nil {
			return err
		}
		return w.PutSearch(ctx, search, current.Revision)
	}); err != nil {
		t.Fatal(err)
	}
	return issue
}
