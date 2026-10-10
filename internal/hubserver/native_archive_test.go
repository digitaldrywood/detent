package hubserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func seedArchiveIssues(t *testing.T, service *Service, scope nativeScope, count int, state string) []tracker.NativeIssue {
	t.Helper()
	tx, err := service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	issues := make([]tracker.NativeIssue, 0, count)
	for i := range count {
		issue, err := createNativeIssueTx(t.Context(), tx, scope, tracker.CreateIssue{Title: fmt.Sprintf("Seed %d", i), Body: nativeContractTestBody, State: state}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		issues = append(issues, issue)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
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
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			ids, err := claimCandidateIDs(t.Context(), tx, claimCandidateQuery{NativeScope: &nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, Scope: string(f.project.ID)}, nil, nil, nil, nil, nil, nil, nil, nil)
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				t.Fatal(rollbackErr)
			}
			if err != nil {
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
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE leases SET expires_at = ? WHERE lease_id = ?", formatHubTime(time.Now().Add(-time.Hour)), lease.ID); err != nil {
					t.Fatal(err)
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
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: "machine", SessionID: "archived-claim", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
				requireNativeStatus(t, response, http.StatusConflict)
			}
		})
	}
}

func TestHostedIssueAllowanceBoundaries(t *testing.T) {
	t.Parallel()
	for _, count := range []int{199, 200, 201} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			t.Parallel()
			f := newHostedSecurityFixture(t)
			owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
			scope := nativeScope{organization: tracker.OrganizationID(f.service.config.Hosted.OrganizationID), project: f.project, credential: apiCredential{ID: bootstrapTokenID, Scope: apiScopeAdmin}}
			seedArchiveIssues(t, f.service, scope, count, "Done")
			response := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create"}, Title: "Allocation", State: "Todo"})
			want := http.StatusTooManyRequests
			if count == 199 {
				want = http.StatusOK
			}
			requireNativeStatus(t, response, want)
			if want == http.StatusTooManyRequests {
				for _, text := range []string{`"resource":"unarchived_issues"`, `"allowance":200`, fmt.Sprintf(`"consumption":%d`, count)} {
					if !strings.Contains(response.Body.String(), text) {
						t.Fatalf("missing contextual count: %s", response.Body.String())
					}
				}
			}
			usage, err := f.service.database.hostedPlanUsage(t.Context(), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if usage.Usage["unarchived_issues"] != int64(max(count, 200)) {
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
	scope := nativeScope{organization: tracker.OrganizationID(f.service.config.Hosted.OrganizationID), project: f.project, credential: apiCredential{ID: bootstrapTokenID, Scope: apiScopeAdmin}}
	issues := seedArchiveIssues(t, f.service, scope, 2, "Done")
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
	request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "new"}, Title: "Replacement", State: "Todo"}
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
	scope := nativeScope{organization: tracker.OrganizationID(f.service.config.Hosted.OrganizationID), project: f.project, credential: apiCredential{ID: bootstrapTokenID, Scope: apiScopeAdmin}}
	issues := seedArchiveIssues(t, f.service, scope, 199, "Done")
	response := f.request(t, owner, http.MethodPost, f.base+"/work-items/"+string(issues[0].WorkItemID)+"/archive", archiveRequest(issues[0], "archive"))
	requireNativeStatus(t, response, http.StatusOK)
	var archived tracker.NativeIssue
	decodeHubResponse(t, response, &archived)
	requireNativeStatus(t, f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "replacement"}, Title: "Replacement", State: "Todo"}), http.StatusOK)
	var group sync.WaitGroup
	responses := make(chan *httptest.ResponseRecorder, 8)
	for index := range 8 {
		group.Go(func() {
			if index == 0 {
				responses <- f.request(t, owner, http.MethodPost, f.base+"/work-items/"+string(archived.WorkItemID)+"/restore", archiveRequest(archived, "restore-race"))
				return
			}
			responses <- f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: strconv.Itoa(index)}, Title: "Contender", State: "Todo"})
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
	if usage.Usage["unarchived_issues"] != 200 {
		t.Fatalf("concurrent usage=%v", usage.Usage)
	}
}

func TestNativeArchiveSelfHostedNoQuota(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{ID: bootstrapTokenID, Scope: apiScopeAdmin}}
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
	backend := &scriptedReconcileBackend{steps: []reconcileStep{{snapshot: ReconcileSnapshot{Repository: RepositorySource{NodeID: "R_repo", Owner: "digitaldrywood", Name: "detent", UpdatedAt: time.Now().UTC()}}}}}
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), ReconcileBackend: backend, ImportBackend: &importFixtureBackend{}})
	f := newNativeFixture(t, service, "", "import-quota")
	d := service.database
	d.hostedOrganization = f.project.OrganizationID
	config := pilotHostedPlans()
	config.Plans[0].Allowances["unarchived_issues"] = 1
	if err := d.configureHostedPlans(t.Context(), &HostedConfig{Plans: &config}); err != nil {
		t.Fatal(err)
	}
	response := performHubAPIRequest(t, service, http.MethodPost, f.base+"/integration/repository", testHubAdminToken, map[string]any{"idempotency_key": "bind", "expected_revision": "1", "repository": "digitaldrywood/detent"})
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
	if err := d.db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE project_id = ?", f.project.ID).Scan(&count); err != nil || count != 1 {
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
	scope := nativeScope{organization: tracker.OrganizationID(f.service.config.Hosted.OrganizationID), project: f.project, credential: apiCredential{ID: bootstrapTokenID, Scope: apiScopeAdmin}}
	first := seedArchiveIssues(t, f.service, scope, 1, "Done")[0]
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
	request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "allocate"}, Title: "Across projects", State: "Todo"}
	requireNativeStatus(t, f.request(t, owner, http.MethodPost, base+"/work-items", request), http.StatusTooManyRequests)
	viewer := f.user(t, "viewer", "viewer", "viewer@example.test", "read", "")
	requireNativeStatus(t, f.request(t, viewer, http.MethodPost, f.base+"/work-items/"+string(first.WorkItemID)+"/archive", archiveRequest(first, "viewer-archive")), http.StatusNotFound)
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
		{"legacy default", nil, 200},
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
			plan, err := readHostedPlan(t.Context(), f.service.database.db, config.Base)
			if err != nil {
				t.Fatal(err)
			}
			if got := plan.Allowances["unarchived_issues"]; got != test.want {
				t.Fatalf("issue allowance = %d, want %d", got, test.want)
			}
			var raw string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT record_json FROM hosted_plans WHERE id = ? AND version = ?", config.Base.ID, config.Base.Version).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if test.configured == nil && strings.Contains(raw, "unarchived_issues") {
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
