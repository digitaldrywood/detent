package hubserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func seedArchiveIssues(t *testing.T, service *Service, scope hubscope.NativeScope, count int, state string) []tracker.NativeIssue {
	t.Helper()
	seedNativeFixturePolicy(t, service, string(scope.Organization)+"/"+string(scope.Project))
	project, err := service.readTenantProjectRecord(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	project.Settings.DuplicateChallenge = false
	if err := service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	actor := scope.Actor()
	if actor.PrincipalID == "" {
		actor = tracker.Actor{Kind: "human", PrincipalID: "example-owner"}
	}
	content := eventstore.NewContent(service.runDetailStore())
	issues := make([]tracker.NativeIssue, 0, count)
	for index := range count {
		var number int64
		if err := service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			number, err = r.NextWorkItemNumber(t.Context(), scope.Project)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		now := service.config.now().UTC().Truncate(time.Microsecond)
		issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: tracker.NativeWorkItemID(hubapi.NewID("wi")), Number: int(number), Revision: 1, Profile: "native"}, Title: fmt.Sprintf("Seed %d", index), Body: nativeContractTestBody, State: state, Actor: actor, Labels: []string{}, Assignees: []string{}, CreatedAt: now, UpdatedAt: now, LastActivityAt: now, IssueContract: &issuecontract.State{ConfirmedSections: config.IssueContractSectionDigests(nativeContractTestBody)}}
		for _, configured := range project.States {
			if configured.Name == state {
				issue.Terminal = configured.Terminal
			}
		}
		if issue.Terminal {
			issue.ClosedAt = &now
		}
		fields, err := service.prepareWorkItemFields(t.Context(), scope.Organization, workItemRecordPlan{issue: issue, create: true})
		if err != nil {
			t.Fatal(err)
		}
		version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: 1}, historyTestJSON(t, issue))
		if err != nil {
			t.Fatal(err)
		}
		event, err := content.PrepareEvent(t.Context(), scope.Organization, historyTestJSON(t, tracker.CollaborationEvent{ID: hubapi.NewID("event"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: issue.WorkItemID, Type: "issue.created", SchemaVersion: 1, Actor: actor, RecordedAt: now, Data: tracker.CollaborationData{Revision: 1}}))
		if err != nil {
			t.Fatal(err)
		}
		document, err := searchstore.NewContent(service.runDetailStore()).Prepare(t.Context(), searchstore.Document{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: issue.WorkItemID, ID: "issue:" + string(issue.WorkItemID), SourceID: string(issue.WorkItemID), Kind: "issue", Scope: "issue", Revision: 1, UpdatedAt: now}, searchstore.Text{Title: issue.Title, Body: issue.Body, Labels: issue.Labels})
		if err != nil {
			t.Fatal(err)
		}
		if err := service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateWorkItemWithClaimFacts(t.Context(), corestore.WorkItem{OrganizationID: scope.Organization, ProjectID: scope.Project, NativeID: issue.WorkItemID, Number: number, Revision: 1, Title: issue.Title, BodyObjectKey: &fields.Body, IssueContractObjectKey: fields.Contract, WorkflowStateName: &state, ActorKind: &actor.Kind, ActorPrincipalID: &actor.PrincipalID, CreatedAt: now, UpdatedAt: now, NativeCreatedAt: now, NativeUpdatedAt: now, LastActivityAt: now}, nil, claimstore.WorkItemFacts{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: issue.WorkItemID, Author: actor.PrincipalID}, claimstore.QueueEntry{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: issue.WorkItemID, Scope: string(scope.Project), State: state, Rank: string(issue.WorkItemID), CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
				return err
			}
			if err := w.PutSearch(t.Context(), document, 0); err != nil {
				return err
			}
			_, err := w.AppendEvent(t.Context(), event)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		issues = append(issues, issue)
	}
	return issues
}

func archiveRequest(issue tracker.NativeIssue, key string) archiveIssueRequest {
	return archiveIssueRequest{Mutation: tracker.Mutation{IdempotencyKey: key}, ExpectedRevision: issue.Revision}
}

func TestNativeArchiveLifecycle(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	issue := f.create(t, "History retained")
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", f.token, map[string]any{"idempotency_key": "comment", "body": "Retained comment"})
	requireNativeStatus(t, response, http.StatusOK)
	for _, test := range []struct {
		name, action string
		archived     bool
	}{
		{"archive", "archive", true}, {"restore", "restore", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := archiveRequest(issue, test.name)
			response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/"+test.action, f.token, request)
			requireNativeStatus(t, response, http.StatusOK)
			var updated tracker.NativeIssue
			decodeHubResponse(t, response, &updated)
			if updated.Archived != test.archived || updated.Revision != issue.Revision+1 || updated.State != issue.State || updated.Body != issue.Body {
				t.Fatalf("updated issue = %+v", updated)
			}
			repeated := performHubAPIRequest(t, f.service, http.MethodPost, path+"/"+test.action, f.token, request)
			requireNativeStatus(t, repeated, http.StatusOK)
			if repeated.Body.String() != response.Body.String() {
				t.Fatal("idempotent response changed")
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/"+test.action, f.token, archiveRequest(issue, "stale-"+test.name)), http.StatusConflict)
			drainTestNativeBoard(t, f.service)
			for _, query := range []string{"", "?archived=true", "?archived=all"} {
				response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items"+query, f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var page tracker.Page[tracker.NativeIssue]
				decodeHubResponse(t, response, &page)
				want := 0
				if query == "?archived=all" || (query == "?archived=true") == test.archived {
					want = 1
				}
				if len(page.Items) != want {
					t.Fatalf("%s list = %d, want %d", query, len(page.Items), want)
				}
			}
			for _, resource := range []string{"", "/comments", "/history", "/attempts", "/versions/1"} {
				response := performHubAPIRequest(t, f.service, http.MethodGet, path+resource, f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				if resource == "/comments" && !strings.Contains(response.Body.String(), "Retained comment") {
					t.Fatal("comment lost")
				}
				if resource == "/history" && !strings.Contains(response.Body.String(), `"operation":"archive"`) {
					t.Fatal("archive audit lost")
				}
			}
			scope := analyticsTestScope(t, f)
			var ids []tracker.NativeWorkItemID
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				if err := f.service.authorizeAccessScope(t.Context(), r, scope, false); err != nil {
					return err
				}
				page, err := r.CandidatePage(t.Context(), claimstore.CandidateQuery{ProjectID: f.project.ID, Scope: string(f.project.ID), AvailableAt: f.service.config.now(), Limit: corestore.MaxPageSize})
				for _, candidate := range page.Items {
					ids = append(ids, candidate.WorkItemID)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if (len(ids) == 0) != test.archived {
				t.Fatalf("candidate count=%d archived=%v", len(ids), test.archived)
			}
			issue = updated
		})
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?archived=bad", f.token, nil), http.StatusUnprocessableEntity)
	worker := f.worker(t, "archive-worker")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/archive", worker, archiveRequest(issue, "worker-archive")), http.StatusUnprocessableEntity)
}

func TestNativeArchiveActiveWork(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name             string
		started, expired bool
	}{
		{"live claim", false, false}, {"running attempt", true, false}, {"running attempt after lease expiry", true, true}, {"expired claim", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newDefaultNativeFixture(t, Config{})
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			issue := f.create(t, "Active work")
			worker := f.worker(t, "worker")
			lease := claimNativeAttempt(t, f, worker, "machine", "session", issue.WorkItemID)
			path := f.base + "/work-items/" + string(issue.WorkItemID)
			start := nativeStartedEvent(lease)
			if test.started {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, start), http.StatusOK)
			}
			if test.expired {
				now := f.service.config.now().UTC()
				result, err := nativeAttemptTestDatabase(t, f).ExecContext(t.Context(), "UPDATE leases SET acquired_at=$1,renewed_at=$2,expires_at=$3 WHERE organization_id=$4 AND lease_id=$5 AND fencing_token=$6", now.Add(-3*time.Hour), now.Add(-2*time.Hour), now.Add(-time.Hour), f.project.OrganizationID, lease.ID, int64(lease.FencingToken))
				if err != nil {
					t.Fatal(err)
				}
				if count, err := result.RowsAffected(); err != nil || count != 1 {
					t.Fatalf("expired lease fixture affected%d rows: %v", count, err)
				}
			}
			status := http.StatusConflict
			if test.expired {
				status = http.StatusOK
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/archive", f.token, archiveRequest(issue, "archive"))
			requireNativeStatus(t, response, status)
			if status == http.StatusConflict {
				if got := readWorkItem(t, f, issue.WorkItemID, ""); got.Archived || got.Revision != issue.Revision {
					t.Fatal("refusal modified issue")
				}
			}
			if test.started && !test.expired {
				finish := start
				finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", "finish", 2, "cancelled"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, finish), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/archive", f.token, archiveRequest(issue, "before-release")), http.StatusConflict)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, map[string]any{"fencing_token": fmt.Sprint(lease.FencingToken), "reason": "completed"}), http.StatusNoContent)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/archive", f.token, archiveRequest(issue, "after-finish")), http.StatusOK)
				response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts", f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				if !strings.Contains(response.Body.String(), start.Data.AttemptID) {
					t.Fatal("attempt lost")
				}
			}
			if status == http.StatusOK {
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: lease.MachineID, SessionID: "archived-claim", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
				requireNativeStatus(t, response, http.StatusConflict)
			}
		})
	}
}

func TestHostedIssueAllowanceBoundaries(t *testing.T) {
	t.Parallel()
	for _, count := range []int{2, 3, 4} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			t.Parallel()
			f := newHostedSecurityFixture(t)
			owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
			hostedTestPlans(t, f.service, map[string]int64{"unarchived_issues": 3})
			scope := hubscope.NativeScope{Organization: tracker.OrganizationID(f.service.config.Hosted.OrganizationID), Project: f.project, Credential: hubapi.APICredential{ID: bootstrapTokenID, Scope: hubapi.APIScopeAdmin}}
			seedArchiveIssues(t, f.service, scope, count, "Todo")
			seedArchiveIssues(t, f.service, scope, 2, "Done")
			response := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Body: nativeContractTestBody, Mutation: tracker.Mutation{IdempotencyKey: "create"}, Title: "Allocation", State: "Todo"})
			want := http.StatusTooManyRequests
			if count == 2 {
				want = http.StatusOK
			}
			requireNativeStatus(t, response, want)
			if want == http.StatusTooManyRequests {
				for _, text := range []string{`"resource":"unarchived_issues"`, `"allowance":3`, fmt.Sprintf(`"consumption":%d`, count)} {
					if !strings.Contains(response.Body.String(), text) {
						t.Fatalf("missing contextual count: %s", response.Body.String())
					}
				}
			}
			usage, err := f.service.database.hostedPlanUsage(t.Context(), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if usage.Usage["unarchived_issues"] != int64(max(count, 3)) {
				t.Fatalf("usage=%v", usage.Usage)
			}
		})
	}
}

func TestHostedIssueArchiveAndDowngrade(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
	config := hostedTestPlans(t, f.service, map[string]int64{"unarchived_issues": 2})
	scope := hubscope.NativeScope{Organization: tracker.OrganizationID(f.service.config.Hosted.OrganizationID), Project: f.project, Credential: hubapi.APICredential{ID: bootstrapTokenID, Scope: hubapi.APIScopeAdmin}}
	issues := seedArchiveIssues(t, f.service, scope, 2, "Todo")
	issuePath := func(issue tracker.NativeIssue) string { return f.base + "/work-items/" + string(issue.WorkItemID) }
	action := func(issue tracker.NativeIssue, op, key string, want int) tracker.NativeIssue {
		t.Helper()
		response := f.request(t, owner, http.MethodPost, issuePath(issue)+"/"+op, archiveRequest(issue, key))
		requireNativeStatus(t, response, want)
		if want != http.StatusOK {
			return issue
		}
		var result tracker.NativeIssue
		decodeHubResponse(t, response, &result)
		return result
	}
	archived := action(issues[0], "archive", "archive", http.StatusOK)
	request := tracker.CreateIssue{Body: nativeContractTestBody, Mutation: tracker.Mutation{IdempotencyKey: "new"}, Title: "Replacement", State: "Todo"}
	response := f.request(t, owner, http.MethodPost, f.base+"/work-items", request)
	requireNativeStatus(t, response, http.StatusOK)
	var replacement tracker.NativeIssue
	decodeHubResponse(t, response, &replacement)
	action(archived, "restore", "restore-full", http.StatusTooManyRequests)
	action(replacement, "archive", "archive-replacement", http.StatusOK)
	restored := action(archived, "restore", "restore", http.StatusOK)
	plan := config.Plans[0]
	plan.Version++
	plan.Allowances = map[string]int64{"unarchived_issues": 0}
	plan.Features = []string{"collaboration"}
	config.Plans = append(config.Plans, plan)
	cfg := *f.service.config.Hosted
	cfg.Plans = &config
	if err := f.service.database.configureHostedPlans(t.Context(), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.applyHostedPlanCommand(t.Context(), bootstrapTokenID, hostedPlanCommand{ID: "downgrade", Action: "base", ExpectedRevision: 1, Plan: plan.PlanReference, Reason: "test downgrade"}); err != nil {
		t.Fatal(err)
	}
	archived = action(restored, "archive", "after-downgrade", http.StatusOK)
	action(archived, "archive", "already-archived", http.StatusOK)
	for _, resource := range []string{"", "/history", "/versions/1", "/comments", "/attempts"} {
		requireNativeStatus(t, f.request(t, owner, http.MethodGet, issuePath(archived)+resource, nil), http.StatusOK)
	}
	action(archived, "restore", "after-downgrade-restore", http.StatusTooManyRequests)
	usage, err := f.service.database.hostedPlanUsage(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if usage.Usage["unarchived_issues"] != 1 {
		t.Fatalf("downgrade usage=%v", usage.Usage)
	}
}

func TestHostedIssueConcurrentAllocation(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
	hostedTestPlans(t, f.service, map[string]int64{"unarchived_issues": 5})
	scope := hubscope.NativeScope{Organization: tracker.OrganizationID(f.service.config.Hosted.OrganizationID), Project: f.project, Credential: hubapi.APICredential{ID: bootstrapTokenID, Scope: hubapi.APIScopeAdmin}}
	issues := seedArchiveIssues(t, f.service, scope, 4, "Todo")
	response := f.request(t, owner, http.MethodPost, f.base+"/work-items/"+string(issues[0].WorkItemID)+"/archive", archiveRequest(issues[0], "archive"))
	requireNativeStatus(t, response, http.StatusOK)
	var archived tracker.NativeIssue
	decodeHubResponse(t, response, &archived)
	requireNativeStatus(t, f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Body: nativeContractTestBody, Mutation: tracker.Mutation{IdempotencyKey: "replacement"}, Title: "Replacement", State: "Todo"}), http.StatusOK)
	var group sync.WaitGroup
	responses := make(chan *httptest.ResponseRecorder, 8)
	for index := range 8 {
		group.Go(func() {
			if index == 0 {
				responses <- f.request(t, owner, http.MethodPost, f.base+"/work-items/"+string(archived.WorkItemID)+"/restore", archiveRequest(archived, "restore-race"))
				return
			}
			responses <- f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Body: nativeContractTestBody, Mutation: tracker.Mutation{IdempotencyKey: strconv.Itoa(index)}, Title: "allocationfixture" + strconv.Itoa(index) + "x", State: "Todo"})
		})
	}
	group.Wait()
	close(responses)
	winners := 0
	for response := range responses {
		if response.Code == http.StatusOK {
			winners++
		} else {
			requireNativeStatus(t, response, http.StatusTooManyRequests)
		}
	}
	if winners != 1 {
		t.Fatalf("winners=%d", winners)
	}
	usage, err := f.service.database.hostedPlanUsage(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if usage.Usage["unarchived_issues"] != 5 {
		t.Fatalf("concurrent usage=%v", usage.Usage)
	}
}

func TestNativeArchiveSelfHostedNoQuota(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: hubapi.APICredential{ID: bootstrapTokenID, Scope: hubapi.APIScopeAdmin}}
	seedArchiveIssues(t, f.service, scope, 201, "Done")
	issue := f.create(t, "Beyond hosted allowance")
	for _, op := range []string{"archive", "restore"} {
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/"+op, f.token, archiveRequest(issue, op))
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &issue)
	}
}

func TestHostedIssueImportAllocation(t *testing.T) {
	t.Parallel()
	backend := &scriptedReconcileBackend{steps: []reconcileStep{{snapshot: ReconcileSnapshot{Repository: RepositorySource{NodeID: "R_repo", Owner: "example-org", Name: "example-project", UpdatedAt: time.Now().UTC()}}}}}
	service := newPostgresNativeService(t, Config{ReconcileBackend: backend, ImportBackend: &importFixtureBackend{}})
	f := newNativeFixture(t, service, "", "import-quota")
	d := service.database
	d.hostedOrganization = f.project.OrganizationID
	config := pilotHostedPlans()
	config.Plans[0].Allowances["unarchived_issues"] = 1
	if err := d.configureHostedPlans(t.Context(), &HostedConfig{Plans: &config}); err != nil {
		t.Fatal(err)
	}
	response := performHubAPIRequest(t, service, http.MethodPost, f.base+"/integration/repository", testHubAdminToken, map[string]any{"idempotency_key": "bind", "expected_revision": "1", "repository": "example-org/example-project"})
	requireNativeStatus(t, response, http.StatusOK)
	var integration ProjectIntegration
	decodeHubResponse(t, response, &integration)
	response = performHubAPIRequest(t, service, http.MethodPut, f.base+"/integration", testHubAdminToken, map[string]any{"idempotency_key": "intake", "expected_revision": fmt.Sprint(integration.Revision), "intake": "manual", "projection": "disabled", "repository_enabled": false})
	requireNativeStatus(t, response, http.StatusOK)
	first := advanceImportFixture(t, f, startImportFixture(t, f, 1, false, 0))
	second := startImportFixture(t, f, 2, false, 0)
	response = performHubAPIRequest(t, service, http.MethodPost, f.base+"/imports/"+second.ID+"/advance", f.token, map[string]any{"expected_revision": fmt.Sprint(second.Revision)})
	requireNativeStatus(t, response, http.StatusTooManyRequests)
	var count int
	if err := usagePostgresAdmin(t, service).QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("import rejection count=%d err=%v", count, err)
	}
	item := readWorkItem(t, f, tracker.NativeWorkItemID(first.WorkItemID), "")
	response = performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items/"+first.WorkItemID+"/archive", f.token, archiveRequest(item, "archive-import"))
	requireNativeStatus(t, response, http.StatusOK)
	second = advanceImportFixture(t, f, second)
	if second.WorkItemID == "" || second.WorkItemID == first.WorkItemID {
		t.Fatalf("second import=%+v", second)
	}
	first = finishImportFixture(t, f, first)
	first = advanceImportFixture(t, f, startImportFixture(t, f, 1, true, first.Revision))
	if !readWorkItem(t, f, tracker.NativeWorkItemID(first.WorkItemID), "").Archived {
		t.Fatal("reimport restored archived issue")
	}
	usage, err := d.hostedPlanUsage(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if usage.Usage["unarchived_issues"] != 1 {
		t.Fatalf("import usage=%v", usage.Usage)
	}
}

func TestHostedIssueAllowanceAcrossProjects(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
	hostedTestPlans(t, f.service, map[string]int64{"unarchived_issues": 1})
	scope := hubscope.NativeScope{Organization: tracker.OrganizationID(f.service.config.Hosted.OrganizationID), Project: f.project, Credential: hubapi.APICredential{ID: bootstrapTokenID, Scope: hubapi.APIScopeAdmin}}
	seedArchiveIssues(t, f.service, scope, 2, "Done")
	usage, err := f.service.database.hostedPlanUsage(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if usage.Usage["unarchived_issues"] != 0 {
		t.Fatalf("terminal issues counted as open: %v", usage.Usage["unarchived_issues"])
	}
	first := seedArchiveIssues(t, f.service, scope, 1, "Todo")[0]
	response := f.request(t, owner, http.MethodPost, "/api/v2/organizations/org_security/projects", map[string]any{"idempotency_key": "second-project", "name": "Second project", "grant_access": true})
	requireNativeStatus(t, response, http.StatusCreated)
	var created struct {
		ProjectID string `json:"project_id"`
	}
	decodeHubResponse(t, response, &created)
	if created.ProjectID == "" {
		t.Fatalf("project response=%s", response.Body.String())
	}
	base := "/api/v2/organizations/org_security/projects/" + created.ProjectID
	seedNativeFixturePolicy(t, f.service, "org_security/"+created.ProjectID)
	request := tracker.CreateIssue{Body: nativeContractTestBody, Mutation: tracker.Mutation{IdempotencyKey: "allocate"}, Title: "Across projects", State: "Todo"}
	requireNativeStatus(t, f.request(t, owner, http.MethodPost, base+"/work-items", request), http.StatusTooManyRequests)
	viewer := f.user(t, "viewer", "viewer", "viewer@example.test", "read", "")
	requireNativeStatus(t, f.request(t, viewer, http.MethodPost, f.base+"/work-items/"+string(first.WorkItemID)+"/archive", archiveRequest(first, "viewer-archive")), http.StatusForbidden)
	requireNativeStatus(t, f.request(t, owner, http.MethodPost, f.base+"/work-items/"+string(first.WorkItemID)+"/archive", archiveRequest(first, "archive")), http.StatusOK)
	requireNativeStatus(t, f.request(t, owner, http.MethodPost, base+"/work-items", request), http.StatusOK)
}

func TestHostedIssueAllowanceCatalog(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		configured *int64
		want       int64
	}{
		{"legacy default", nil, 10000},
		{"explicit zero", new(int64(0)), 0},
		{"explicit paid allowance", new(int64(1000)), 1000},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newHostedSecurityFixture(t)
			limits := make(map[string]int64)
			if test.configured != nil {
				limits["unarchived_issues"] = *test.configured
			}
			config := hostedTestPlans(t, f.service, limits)
			cfg := *f.service.config.Hosted
			cfg.Plans = &config
			if err := f.service.database.configureHostedPlans(t.Context(), &cfg); err != nil {
				t.Fatal(err)
			}
			storedPlan := func() billingstore.Plan {
				t.Helper()
				var stored billingstore.Plan
				if err := f.service.database.readBillingTenant(t.Context(), func(r tenantstore.Reader) error {
					var err error
					stored, err = r.BillingPlan(t.Context(), billingstore.PlanReference{ID: config.Base.ID, Version: config.Base.Version})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return stored
			}
			before := storedPlan()
			plan, err := f.service.database.readHostedPlan(t.Context(), nil, config.Base)
			if err != nil {
				t.Fatal(err)
			}
			if got := plan.Allowances["unarchived_issues"]; got != test.want {
				t.Fatalf("issue allowance = %d, want %d", got, test.want)
			}
			after := storedPlan()
			if !reflect.DeepEqual(before, after) {
				t.Fatal("reading catalog changed its stored typed plan")
			}
			if _, exists := after.Allowances["unarchived_issues"]; test.configured == nil && exists {
				t.Fatal("reading legacy catalog rewrote its immutable version")
			}
		})
	}
}

func TestNativeArchiveRetainsChanges(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t, nil)
	version := f.publish(t, "version", "")
	path := f.base + "/work-items/" + string(f.issue.WorkItemID)
	issue := readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, "")
	for _, op := range []string{"archive", "restore"} {
		response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/"+op, f.token, archiveRequest(issue, op))
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &issue)
		for _, resource := range []string{path + "/changes", f.path} {
			response := performHubAPIRequest(t, f.service, http.MethodGet, resource, f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			if !strings.Contains(response.Body.String(), f.change.ID) {
				t.Fatal("archiving lost the retained change")
			}
		}
		detail := f.detail(t)
		if len(detail.Versions) != 1 || detail.Versions[0].ID != version.ID || detail.Versions[0].HeadSHA != version.HeadSHA {
			t.Fatalf("retained versions = %+v", detail.Versions)
		}
	}
}
