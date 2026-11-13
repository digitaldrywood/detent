package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	connectorgithub "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func newPostgresExportLinkedFixture(t *testing.T) (tenantstore.Repository, hostedSecurityFixture, hostedSecurityUser, map[tracker.NativeWorkItemID]string, map[tracker.NativeWorkItemID]string) {
	t.Helper()
	repository, f, owner, admin := newExportPostgresFixture(t)
	expected := map[tracker.NativeWorkItemID]string{}
	changes := map[tracker.NativeWorkItemID]string{}
	for _, name := range []string{"first", "second"} {
		response := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: name}, Title: name, Body: workItemRecordBody, State: "Todo"})
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		expected[issue.WorkItemID] = name
		response = f.request(t, owner, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment-" + name}, Body: name + " discussion"})
		requireNativeStatus(t, response, http.StatusOK)
		response = f.request(t, owner, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/changes", tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change-" + name}, Title: name + " change", Body: "Change detail"})
		requireNativeStatus(t, response, http.StatusOK)
		var change tracker.ChangeRequest
		decodeHubResponse(t, response, &change)
		changes[issue.WorkItemID] = change.ID
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO release_deliveries(organization_id,project_id,work_item_id,source_issue_id,source_change_id,source_merge_sha,repository,pr_number,head_ref,head_sha,base_ref,base_sha,frozen_sha,change_id,principal_id,actor_kind,via_key,associated_at) VALUES('org_security',$1,$2,$2,$3,'source','https://github.com/example-org/example-project',$4,'fix','head','release/v1.1','base','frozen',$3,'example','human','',now())`, f.project, issue.WorkItemID, change.ID, len(expected)); err != nil {
			t.Fatal(err)
		}
	}
	return repository, f, owner, expected, changes
}

func TestPostgresLinkedChanges(t *testing.T) {
	repository, f, owner, expected, changes := newPostgresExportLinkedFixture(t)
	t.Run("linked", func(t *testing.T) {
		for id, name := range expected {
			response := f.request(t, owner, http.MethodGet, f.base+"/work-items/"+string(id)+"/pull-requests", nil)
			requireNativeStatus(t, response, http.StatusOK)
			var views []pullRequestView
			decodeHubResponse(t, response, &views)
			if len(views) != 1 || views[0].Title != name+" change" || views[0].Number < 1 || views[0].Head.SHA != "head" || views[0].Base.Ref != "release/v1.1" || views[0].Connector != nil || views[0].Checks == nil || views[0].Reviews == nil || views[0].Labels == nil {
				t.Fatalf("linked changes=%+v", views)
			}
		}
	})
	t.Run("current_version", func(t *testing.T) {
		for id, changeID := range changes {
			for number := int64(1); number <= 2; number++ {
				input := changeTestInput()
				input.AttemptID = "attempt_" + string(id)
				if number == 2 {
					input.External = &tracker.ChangeExternalReference{Provider: "github", ID: "42", URL: "https://github.com/example-org/example-project/pull/42"}
				}
				content := eventstore.NewContent(f.service.runDetailStore())
				source, err := content.PrepareSourceBundle(t.Context(), "org_security", changeTestSource(t, &input))
				if err != nil {
					t.Fatal(err)
				}
				version := tracker.ChangeVersion{ID: fmt.Sprintf("version_%s_%d", id, number), ChangeID: changeID, Number: number, ChangeVersionInput: input, Policy: hubTestPolicy(), ReviewPolicy: defaultChangeReviewPolicy(hubTestPolicy()), Actor: tracker.Actor{Kind: "human", PrincipalID: owner.identity.Subject}, CreatedAt: f.service.config.now()}
				version.PolicyID = version.Policy.ID
				raw, err := json.Marshal(version)
				if err != nil {
					t.Fatal(err)
				}
				row, err := content.PrepareVersion(t.Context(), "org_security", raw)
				if err != nil {
					t.Fatal(err)
				}
				row.SourceObjectKey, row.SourceSHA256, row.SourceBytes, row.SourceContentBytes = source.Key, source.SHA256, source.CompressedSize, source.UncompressedSize
				if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.CreateVersion(t.Context(), row); err != nil {
						return err
					}
					change, err := w.Change(t.Context(), changeID)
					if err != nil {
						return err
					}
					change.CurrentVersionID = new(version.ID)
					_, err = w.UpdateChange(t.Context(), change)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				f.service.pullRequests.invalidate(pullRequestCacheKey(nativeScope{organization: "org_security", project: f.project}, string(id)))
				response := f.request(t, owner, http.MethodGet, f.base+"/work-items/"+string(id)+"/pull-requests", nil)
				requireNativeStatus(t, response, http.StatusOK)
				var views []pullRequestView
				decodeHubResponse(t, response, &views)
				if len(views) != 1 || views[0].Head.SHA != input.HeadSHA || views[0].Base.SHA != input.BaseSHA || views[0].Author != owner.identity.Subject {
					t.Fatalf("current source lost: %+v", views)
				}
				stale := tracker.ChangeRequest{ID: changeID, Title: "stale title", CurrentVersion: "stale version"}
				current, err := f.service.changePullRequestView(t.Context(), nativeScope{organization: "org_security", project: f.project}, string(id), stale, nil, 0, f.service.config.now())
				if err != nil {
					t.Fatal(err)
				}
				if current.Title != views[0].Title || current.Head.SHA != input.HeadSHA || current.URL != views[0].URL {
					t.Fatalf("mixed Change revisions: %+v", current)
				}
				if number == 2 && (views[0].Number != 42 || views[0].URL != input.External.URL) {
					t.Fatalf("external reference lost: %+v", views)
				}
			}
		}
	})
	t.Run("refresh", func(t *testing.T) {
		now := f.service.config.now()
		f.service.config.now = func() time.Time { return now }
		for id := range expected {
			path := f.base + "/work-items/" + string(id) + "/pull-requests"
			requireNativeStatus(t, f.request(t, owner, http.MethodGet, path+"?refresh=1", nil), http.StatusOK)
			requireNativeCode(t, f.request(t, owner, http.MethodGet, path+"?refresh=1", nil), http.StatusTooManyRequests, "refresh_limited")
			now = now.Add(pullRequestRefreshInterval)
			requireNativeStatus(t, f.request(t, owner, http.MethodGet, path+"?refresh=1", nil), http.StatusOK)
			requireNativeCode(t, f.request(t, owner, http.MethodGet, path+"?refresh=2", nil), http.StatusUnprocessableEntity, "invalid_request")
			now = now.Add(pullRequestRefreshInterval)
		}
	})

	t.Run("connected_refresh", func(t *testing.T) {
		admin := postgresStartupTemplate.Database(t)
		for index, org := range []tracker.OrganizationID{"org_refresh_first", "org_refresh_second"} {
			t.Run(string(org), func(t *testing.T) {
				var clock atomic.Int64
				clock.Store(time.Now().UTC().Truncate(time.Microsecond).UnixNano())
				now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
				advance := func() { clock.Add(int64(pullRequestRefreshInterval)) }
				calls, linkedCalls := 0, 0
				var targetedFailure, linkedFailure error
				providerHeadOverride := ""
				allowLinked := false
				var duringFetch func()
				backend := pullRequestRefreshBackend(func(ctx context.Context, request ReconcileRequest) (ReconcileSnapshot, error) {
					if !request.TargetedPullRequests {
						if !allowLinked || !request.SkipIssues || !request.SkipRepository || len(request.Hydrations) != 0 {
							t.Fatalf("unexpected repository reconciliation=%+v", request)
						}
						linkedCalls++
						return ReconcileSnapshot{}, linkedFailure
					}
					calls++
					if targetedFailure != nil {
						return ReconcileSnapshot{}, targetedFailure
					}
					if !request.TargetedPullRequests || len(request.Hydrations) != 1 || request.Hydrations[0].ObjectKind != "pull_request" || request.Repository.Owner != "example-org" || request.Repository.Name != "example-project" {
						t.Fatalf("refresh enumerated repository or wrong request: %+v", request)
					}
					target := request.Hydrations[0]
					if duringFetch != nil {
						hook := duringFetch
						duringFetch = nil
						hook()
					}
					observedHead := target.HeadSHA
					if providerHeadOverride != "" {
						observedHead = providerHeadOverride
					}
					state := "closed"
					if index == 1 {
						state = "open"
					}
					mergeable := "dirty"
					return ReconcileSnapshot{Repository: RepositorySource{NodeID: "R_example", Owner: "example-org", Name: "example-project", UpdatedAt: now()}, PullRequests: []PullRequestSource{{Number: target.GitHubNumber, HeadSHA: observedHead, State: state, Draft: true, UpdatedAt: now()}}, PullRequestDetails: []PullRequestDetailSource{{Number: target.GitHubNumber, HeadSHA: observedHead, MergeableState: &mergeable, Checks: &tracker.CheckSummary{Status: "completed", Conclusion: "success", Total: 2, Passed: 2}, Reviews: &tracker.ReviewSummary{State: "approved", Decision: "approved", Approvals: 1}}}}, nil
				})
				repository, f, owner, _ := newExportPostgresFixtureInOrganization(t, admin, org, func(cfg *Config) {
					cfg.now = now
					cfg.ReconcileBackend = backend
					cfg.jitter = func() float64 { return 0.5 }
				})
				write := func(fn func(tenantstore.Writer) error) {
					t.Helper()
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, fn); err != nil {
						t.Fatal(err)
					}
				}
				secondProject := tracker.ProjectID("project_other")
				otherPolicy, err := eventstore.NewContent(f.service.runDetailStore()).PrepareProjectPolicy(t.Context(), org, hubTestPolicy())
				if err != nil {
					t.Fatal(err)
				}
				otherPolicy.ProjectID, otherPolicy.ApprovedBy, otherPolicy.ApprovedAt = secondProject, owner.identity.Subject, now()
				viewer := f.user(t, "refresh_viewer", "member", "refresh-viewer@example.test", "", "")
				write(func(w tenantstore.Writer) error {
					if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: secondProject, Name: "Other example", Slug: "other-example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
						return err
					}
					if err := w.ApproveProjectPolicy(t.Context(), otherPolicy, ""); err != nil {
						return err
					}
					for _, project := range []tracker.ProjectID{f.project, secondProject} {
						row, err := w.ProjectRecord(t.Context(), project)
						if err != nil {
							return err
						}
						row.Settings.CheckoutRepository, row.Settings.RepositoryEnabled = "example-org/example-project", true
						if _, err = w.UpdateProjectRecord(t.Context(), row); err != nil {
							return err
						}
						for _, user := range []hostedSecurityUser{owner, viewer} {
							member, err := w.AccessMember(t.Context(), user.identity.Subject)
							if err != nil {
								return err
							}
							if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: member.UserID, ProjectID: project, CanWrite: true}); err != nil {
								return err
							}
							if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: member.PrincipalID, ProjectID: project}); err != nil {
								return err
							}
						}
					}
					return nil
				})
				checkout := func(project tracker.ProjectID) {
					t.Helper()
					worker := nativeFixture{service: f.service, project: tracker.NativeProject{ID: project, OrganizationID: org}}
					token := worker.worker(t, "PR refresh "+string(project))
					credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), org, apikey.HashToken(token), "", "")
					if err != nil || status != http.StatusOK {
						t.Fatalf("worker authentication=%d %v", status, err)
					}
					write(func(w tenantstore.Writer) error {
						return w.ReplaceRunnerCheckoutReport(t.Context(), runnerstore.CheckoutReport{OrganizationID: org, RunnerID: string(credential.Runner.Binding.RunnerID), ProjectID: project, Repository: "example-org/example-project", ReportedAt: now()})
					})
				}
				checkout(f.project)
				checkout(secondProject)
				type linkedView struct {
					fixture       hostedSecurityFixture
					item          tracker.NativeWorkItemID
					change        string
					version       string
					versionNumber int64
					head          string
				}
				create := func(project tracker.ProjectID, key, head string) linkedView {
					t.Helper()
					target := f
					target.project = project
					target.base = "/api/v2/organizations/" + string(org) + "/projects/" + string(project)
					response := target.request(t, owner, http.MethodPost, target.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "refresh_item_" + key}, Title: "Example " + key, Body: workItemRecordBody, State: "Todo"})
					requireNativeStatus(t, response, http.StatusOK)
					var issue tracker.NativeIssue
					decodeHubResponse(t, response, &issue)
					response = target.request(t, owner, http.MethodPost, target.base+"/work-items/"+string(issue.WorkItemID)+"/changes", tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "refresh_change_" + key}, Title: "Example " + key})
					requireNativeStatus(t, response, http.StatusOK)
					var change tracker.ChangeRequest
					decodeHubResponse(t, response, &change)
					return linkedView{fixture: target, item: issue.WorkItemID, change: change.ID, head: head}
				}
				publishVersion := func(view *linkedView, number int64, head string) {
					t.Helper()
					input := changeTestInput()
					input.HeadSHA = head
					input.Repository = "https://github.com/example-org/example-project"
					input.External = &tracker.ChangeExternalReference{Provider: "github", ID: "18", URL: "https://github.com/example-org/example-project/pull/18"}
					content := eventstore.NewContent(f.service.runDetailStore())
					source, err := content.PrepareSourceBundle(t.Context(), org, changeTestSource(t, &input))
					if err != nil {
						t.Fatal(err)
					}
					version := tracker.ChangeVersion{ID: newNativeID("version"), ChangeID: view.change, Number: number, ChangeVersionInput: input, Policy: hubTestPolicy(), ReviewPolicy: defaultChangeReviewPolicy(hubTestPolicy()), Actor: tracker.Actor{Kind: "human", PrincipalID: owner.identity.Subject}, CreatedAt: now()}
					version.PolicyID = version.Policy.ID
					raw, err := json.Marshal(version)
					if err != nil {
						t.Fatal(err)
					}
					row, err := content.PrepareVersion(t.Context(), org, raw)
					if err != nil {
						t.Fatal(err)
					}
					row.SourceObjectKey, row.SourceSHA256, row.SourceBytes, row.SourceContentBytes = source.Key, source.SHA256, source.CompressedSize, source.UncompressedSize
					write(func(w tenantstore.Writer) error {
						if _, err := w.CreateVersion(t.Context(), row); err != nil {
							return err
						}
						change, err := w.Change(t.Context(), view.change)
						if err != nil {
							return err
						}
						change.CurrentVersionID = new(version.ID)
						_, err = w.UpdateChange(t.Context(), change)
						return err
					})
					view.version, view.head, view.versionNumber = version.ID, input.HeadSHA, number
				}
				a, b, c := create(f.project, "a", strings.Repeat("a", 40)), create(f.project, "b", strings.Repeat("b", 40)), create(secondProject, "c", strings.Repeat("c", 40))
				for _, view := range []*linkedView{&a, &b, &c} {
					publishVersion(view, 1, view.head)
				}
				path := func(view linkedView) string {
					return view.fixture.base + "/work-items/" + string(view.item) + "/pull-requests"
				}
				get := func(view linkedView, refresh bool) pullRequestView {
					t.Helper()
					url := path(view)
					if refresh {
						url += "?refresh=1"
					}
					response := view.fixture.request(t, viewer, http.MethodGet, url, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var views []pullRequestView
					decodeHubResponse(t, response, &views)
					if len(views) != 1 {
						t.Fatalf("views=%+v", views)
					}
					return views[0]
				}
				key := func(view linkedView) projectstore.GitHubPullRequestRefreshKey {
					return projectstore.GitHubPullRequestRefreshKey{ProjectID: view.fixture.project, ChangeID: view.change, Repository: "example-org/example-project", Number: 18}
				}
				read := func(view linkedView) projectstore.GitHubPullRequestRefresh {
					t.Helper()
					var row projectstore.GitHubPullRequestRefresh
					if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						row, err = r.(projectstore.GitHubPullRequestRefreshReader).GitHubPullRequestRefresh(t.Context(), key(view))
						return err
					}); err != nil {
						t.Fatal(err)
					}
					return row
				}
				for _, view := range []linkedView{a, b, c} {
					if initial := get(view, true); initial.Connector != nil || initial.Head.SHA != view.head {
						t.Fatalf("unobserved connector=%+v", initial)
					}
					advance()
				}
				if calls != 0 {
					t.Fatal("HTTP refresh performed provider IO")
				}
				bBefore, cBefore := read(b), read(c)
				if err := f.service.maintainGitHubReconciliationRecords(t.Context()); err != nil {
					t.Fatal(err)
				}
				if calls != 1 || !reflect.DeepEqual(read(b), bBefore) || !reflect.DeepEqual(read(c), cBefore) {
					t.Fatal("one maintenance step changed a sibling view or exceeded one fetch")
				}
				f.service.pullRequests.invalidate(pullRequestCacheKey(nativeScope{organization: org, project: f.project}, string(a.item)))
				visible := get(a, false)
				expectedState := "closed"
				if index == 1 {
					expectedState = "open"
				}
				if visible.Connector == nil || visible.Connector.Repository != "example-org/example-project" || visible.State != expectedState || !visible.Draft || !visible.Mergeable.Known || visible.Mergeable.Value || !visible.FetchedAt.Equal(read(a).Observation.ObservedAt) || visible.Head.SHA != a.head || len(visible.Checks) != 0 || len(visible.Reviews) != 0 || visible.Checks == nil || visible.Reviews == nil {
					t.Fatalf("targeted observation not visible without fabricated native facts: %+v", visible)
				}
				if get(b, false).Connector != nil || get(c, false).Connector != nil {
					t.Fatal("another view displayed A's observation")
				}
				duringFetch = func() { advance(); get(b, true) }
				if err := f.service.maintainGitHubReconciliationRecords(t.Context()); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("stale in-flight completion=%v", err)
				}
				if got := read(b); !got.Pending || got.RequestCount != bBefore.RequestCount+1 || got.Observation != nil || got.Attempts != 0 {
					t.Fatalf("stale response changed newer demand=%+v", got)
				}
				if err := f.service.maintainGitHubReconciliationRecords(t.Context()); err != nil {
					t.Fatal(err)
				}
				f.service.pullRequests.invalidate(pullRequestCacheKey(nativeScope{organization: org, project: f.project}, string(b.item)))
				if got := get(b, false); got.Connector == nil || got.Head.SHA != b.head {
					t.Fatalf("B's own observation absent=%+v", got)
				}
				if err := f.service.maintainGitHubReconciliationRecords(t.Context()); err != nil {
					t.Fatal(err)
				}
				f.service.pullRequests.invalidate(pullRequestCacheKey(nativeScope{organization: org, project: secondProject}, string(c.item)))
				if got := get(c, false); got.Connector == nil || got.Head.SHA != c.head {
					t.Fatalf("cross-project observation absent=%+v", got)
				}

				for _, reason := range []string{"source_changed", "provider_head_changed", "definite_provider_refusal"} {
					advance()
					previous := read(a)
					get(a, true)
					queued := read(a)
					if !queued.Pending || queued.RequestCount != previous.RequestCount+1 || !reflect.DeepEqual(queued.Observation, previous.Observation) {
						t.Fatalf("superseded demand did not rearm through HTTP=%+v", queued)
					}
					advance()
					get(b, true)
					advance()
					sibling := read(b)
					var cause error
					expectedCalls := 1
					switch reason {
					case "source_changed":
						publishVersion(&a, a.versionNumber+1, strings.Repeat("e", 40))
						cause = corestore.ErrRevisionConflict
						expectedCalls = 0
					case "provider_head_changed":
						providerHeadOverride = strings.Repeat("f", 40)
					case "definite_provider_refusal":
						cause = errors.New("provider pull request not found")
						targetedFailure = Permanent(cause)
					}
					before := calls
					err := f.service.maintainGitHubReconciliationRecords(t.Context())
					if err == nil || !IsPermanent(err) || cause != nil && !errors.Is(err, cause) || calls != before+expectedCalls {
						t.Fatalf("%s refusal=%v calls=%d want=%d", reason, err, calls-before, expectedCalls)
					}
					providerHeadOverride, targetedFailure = "", nil
					expected := queued
					expected.Pending = false
					if got := read(a); !reflect.DeepEqual(got, expected) {
						t.Fatalf("%s did not preserve superseded observation/source=%+v", reason, got)
					}
					if !reflect.DeepEqual(read(b), sibling) {
						t.Fatalf("%s refusal changed sibling", reason)
					}
					f.service.pullRequests.invalidate(pullRequestCacheKey(nativeScope{organization: org, project: f.project}, string(a.item)))
					if got := get(a, false); got.Connector != nil || got.Head.SHA != a.head {
						t.Fatalf("%s displayed old-head observation=%+v", reason, got)
					}
					before = calls
					if err := f.service.maintainGitHubReconciliationRecords(t.Context()); err != nil {
						t.Fatal(err)
					}
					if calls != before+1 || read(b).Pending || !reflect.DeepEqual(read(a), expected) {
						t.Fatalf("%s did not free next slot for sibling", reason)
					}
					before = calls
					if err := f.service.maintainGitHubReconciliationRecords(t.Context()); err != nil {
						t.Fatal(err)
					}
					if calls != before || !reflect.DeepEqual(read(a), expected) {
						t.Fatalf("%s repeated provider call after supersede", reason)
					}
				}
				advance()
				get(a, true)
				if err := f.service.maintainGitHubReconciliationRecords(t.Context()); err != nil {
					t.Fatal(err)
				}
				f.service.pullRequests.invalidate(pullRequestCacheKey(nativeScope{organization: org, project: f.project}, string(a.item)))
				if got := get(a, false); got.Connector == nil || got.Head.SHA != a.head {
					t.Fatalf("rearmed current demand did not publish=%+v", got)
				}

				advance()
				get(a, true)
				advance()
				get(b, true)
				advance()
				credentialQueued, siblingQueued := read(a), read(b)
				if !credentialQueued.Pending || credentialQueued.Attempts != 0 || credentialQueued.Observation == nil {
					t.Fatalf("credential retry fixture not current and observed=%+v", credentialQueued)
				}
				authenticationFailure := &connectorgithub.StatusError{StatusCode: http.StatusUnauthorized, Err: connectorgithub.ErrAuthenticationFailed}
				targetedFailure = authenticationFailure
				beforeCredential := calls
				err = f.service.maintainGitHubReconciliationRecords(t.Context())
				if !errors.Is(err, authenticationFailure) || IsPermanent(err) || calls != beforeCredential+1 {
					t.Fatalf("credential fault dropped demand or changed classification: %v", err)
				}
				credentialFailed := read(a)
				expectedCredential := credentialQueued
				expectedCredential.Attempts = 1
				expectedCredential.NextAttemptAt = now().Add(2 * time.Second)
				if !reflect.DeepEqual(credentialFailed, expectedCredential) || !reflect.DeepEqual(read(b), siblingQueued) {
					t.Fatalf("credential retry failed pending/source/observation/backoff fences=%+v", credentialFailed)
				}
				targetedFailure = nil
				beforeCredential = calls
				if err := f.service.maintainGitHubReconciliationRecords(t.Context()); err != nil {
					t.Fatal(err)
				}
				if calls != beforeCredential+1 || read(b).Pending || !reflect.DeepEqual(read(a), credentialFailed) {
					t.Fatal("credential retry consumed sibling due slot")
				}
				beforeCredential = calls
				if err := f.service.maintainGitHubReconciliationRecords(t.Context()); err != nil {
					t.Fatal(err)
				}
				if calls != beforeCredential || !reflect.DeepEqual(read(a), credentialFailed) {
					t.Fatal("credential fault retried before stored due time")
				}
				clock.Store(credentialFailed.NextAttemptAt.UnixNano())
				targetedFailure = authenticationFailure
				beforeCredential = calls
				err = f.service.maintainGitHubReconciliationRecords(t.Context())
				if !errors.Is(err, authenticationFailure) || IsPermanent(err) || calls != beforeCredential+1 {
					t.Fatalf("credential fault did not retry at stored due time: %v", err)
				}
				credentialRetried := read(a)
				expectedCredential.Attempts = 2
				expectedCredential.NextAttemptAt = now().Add(4 * time.Second)
				if !reflect.DeepEqual(credentialRetried, expectedCredential) {
					t.Fatalf("credential retry lost observation or capped-backoff state=%+v", credentialRetried)
				}
				targetedFailure = nil
				clock.Store(credentialRetried.NextAttemptAt.UnixNano())
				beforeCredential = calls
				if err := f.service.maintainGitHubReconciliationRecords(t.Context()); err != nil {
					t.Fatal(err)
				}
				recoveredCredential := read(a)
				if calls != beforeCredential+1 || recoveredCredential.Pending || recoveredCredential.Attempts != 0 || recoveredCredential.RequestCount != credentialQueued.RequestCount || recoveredCredential.Observation == nil || recoveredCredential.Observation.HeadSHA != a.head {
					t.Fatalf("repaired credential did not complete original pending request=%+v", recoveredCredential)
				}
				oldAHead := a.head
				publishVersion(&a, a.versionNumber+1, strings.Repeat("d", 40))
				if got := get(a, false); got.Connector != nil || got.Head.SHA != a.head {
					t.Fatalf("cached old head displayed after source changed=%+v", got)
				}
				advance()
				get(a, true)
				beforeRevoke := calls
				beforeDenied := read(a)
				write(func(w tenantstore.Writer) error {
					member, err := w.AccessMember(t.Context(), viewer.identity.Subject)
					if err != nil {
						return err
					}
					if err := w.DeleteAccessTokenGrant(t.Context(), member.PrincipalID, f.project); err != nil {
						return err
					}
					return w.DeleteAccessProjectGrant(t.Context(), member.UserID, f.project)
				})
				response := f.request(t, viewer, http.MethodGet, path(a)+"?refresh=1", nil)
				requireNativeStatus(t, response, http.StatusNotFound)
				if calls != beforeRevoke || !reflect.DeepEqual(read(a), beforeDenied) {
					t.Fatal("unauthorized refresh changed demand or called provider")
				}
				write(func(w tenantstore.Writer) error {
					record, err := w.ProjectRecord(t.Context(), f.project)
					if err != nil {
						return err
					}
					record.Settings.CheckoutRepository = "example-org/other"
					_, err = w.UpdateProjectRecord(t.Context(), record)
					return err
				})
				beforeDead := calls
				if err := f.service.maintainGitHubReconciliationRecords(t.Context()); err != nil {
					t.Fatal(err)
				}
				if got := read(a); got.Pending || got.Observation == nil || got.Observation.HeadSHA != oldAHead {
					t.Fatalf("rebound demand not cancelled with retained observation=%+v", got)
				}
				if calls != beforeDead {
					t.Fatal("dead origin reached provider")
				}
				response = f.request(t, owner, http.MethodGet, path(a), nil)
				requireNativeStatus(t, response, http.StatusOK)
				var rebound []pullRequestView
				decodeHubResponse(t, response, &rebound)
				if len(rebound) != 1 || rebound[0].Connector != nil || rebound[0].Head.SHA != a.head {
					t.Fatalf("rebound project displayed prior connector=%+v", rebound)
				}

				pending := read(c)
				pending.RequestedAt = now()
				write(func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).RequestGitHubPullRequests(t.Context(), []projectstore.GitHubPullRequestRefresh{pending})
				})
				pending = read(c)
				targetedFailure, linkedFailure = errors.New("targeted provider failure"), errors.New("linked provider failure")
				allowLinked = true
				f.service.config.ImportBackend = linkedTestImporter{}
				beforeBudget := calls
				err = f.service.maintainGitHubReconciliationRecords(t.Context())
				if !errors.Is(err, targetedFailure) || !errors.Is(err, linkedFailure) || calls != beforeBudget+1 || linkedCalls != 1 {
					t.Fatalf("maintenance starved a class or exceeded budget: targeted=%d linked=%d err=%v", calls-beforeBudget, linkedCalls, err)
				}
				failed := read(c)
				if !failed.Pending || failed.Attempts != pending.Attempts+1 || !failed.NextAttemptAt.After(now()) || !reflect.DeepEqual(failed.Observation, pending.Observation) {
					t.Fatalf("failed refresh lost observation or retry=%+v", failed)
				}
				f.service.config.ImportBackend = nil
				beforeRetry := calls
				if err := f.service.maintainGitHubReconciliationRecords(t.Context()); err != nil {
					t.Fatal(err)
				}
				if calls != beforeRetry || !reflect.DeepEqual(read(c), failed) {
					t.Fatal("uncertain failure ignored stored backoff")
				}
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					row, err := r.(projectstore.GitHubReconciliationReader).GitHubReconciliation(t.Context(), f.project)
					if err == nil && (row.Attempts != 1 || row.LastFailureAt == nil || !row.NextAttemptAt.After(now())) {
						t.Fatalf("linked failure did not retain independent backoff=%+v", row)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				clock.Store(failed.NextAttemptAt.UnixNano())
				beforeRetry = calls
				err = f.service.maintainGitHubReconciliationRecords(t.Context())
				if !errors.Is(err, targetedFailure) || calls != beforeRetry+1 || !read(c).Pending || read(c).Attempts != failed.Attempts+1 || !reflect.DeepEqual(read(c).Observation, failed.Observation) {
					t.Fatalf("uncertain failure was not retryable at stored due time: %v", err)
				}

			})
		}
	})

}

func newExportPostgresFixture(t *testing.T) (tenantstore.Repository, hostedSecurityFixture, hostedSecurityUser, *sql.DB) {
	t.Helper()
	return newExportPostgresFixtureInOrganization(t, postgresStartupTemplate.Database(t), "org_security")
}

func newExportPostgresFixtureInOrganization(t *testing.T, admin *sql.DB, organization tracker.OrganizationID, configure ...func(*Config)) (tenantstore.Repository, hostedSecurityFixture, hostedSecurityUser, *sql.DB) {
	t.Helper()
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	cfg.Hosted.OrganizationID = string(organization)
	cfg.Hosted.WorkOSOrganizationID = "org_provider"
	plans := pilotHostedPlans()
	cfg.Hosted.Plans = &plans
	cfg.SecretKeys = secretTestKeys(t, "1", "1")
	now := time.Now().UTC().Truncate(time.Microsecond)
	cfg.now = func() time.Time { return now }
	for _, apply := range configure {
		apply(&cfg)
	}
	now = cfg.now()
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	project := tracker.ProjectID("project_example")
	policy, err := eventstore.NewContent(cfg.RunDetails).PrepareProjectPolicy(t.Context(), organization, hubTestPolicy())
	if err != nil {
		t.Fatal(err)
	}
	policy.ProjectID, policy.ApprovedBy, policy.ApprovedAt = project, "example-owner", now
	if err := store.postgres.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example", CreatedAt: now}); err != nil {
			return err
		}
		deployment, generation := cfg.Hosted.deployment()
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: cfg.Hosted.WorkOSOrganizationID, BootstrapSubject: cfg.Hosted.BootstrapSubject, Deployment: deployment, PublicURL: cfg.Hosted.PublicURL, AllocationGeneration: generation}); err != nil {
			return err
		}
		cursor, err := cfg.SecretKeys.Seal(bytes.Repeat([]byte{0x36}, 32), accessstore.SecretAAD(organization, "", accessstore.NativeCursorKeyKind))
		if err != nil {
			return err
		}
		if err := w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Envelope: cursor, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: cursor.Version, RecordedAt: now}); err != nil {
			return err
		}
		if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Example", Slug: "example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
			return err
		}
		return w.ApproveProjectPolicy(t.Context(), policy, "")
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	f := hostedSecurityFixture{service: service, provider: cfg.Hosted.Provider.(*hostedSecurityProvider), project: project, base: "/api/v2/organizations/" + string(organization) + "/projects/" + string(project)}
	owner := f.user(t, "review_admin", "owner", "review-admin@example.test", "", "")
	repository := service.config.TenantRepository
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), owner.identity.Subject)
		if err != nil {
			return err
		}
		if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: member.UserID, ProjectID: project, CanWrite: true}); err != nil {
			return err
		}
		return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: member.PrincipalID, ProjectID: project})
	}); err != nil {
		t.Fatal(err)
	}
	return repository, f, owner, admin
}

type pullRequestRefreshBackend func(context.Context, ReconcileRequest) (ReconcileSnapshot, error)

func (f pullRequestRefreshBackend) Reconcile(ctx context.Context, request ReconcileRequest) (ReconcileSnapshot, error) {
	return f(ctx, request)
}
