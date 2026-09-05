package hubserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type changeFixture struct {
	nativeFixture
	issue  tracker.NativeIssue
	change tracker.ChangeRequest
	path   string
	rules  tracker.ChangeReviewPolicy
}

func newChangeFixture(t *testing.T, service *Service) changeFixture {
	t.Helper()
	f := newNativeFixture(t, service, "", "changes")
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	var principal string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT id FROM api_tokens WHERE name = 'operator-changes'").Scan(&principal); err != nil {
		t.Fatal(err)
	}
	rules := tracker.ChangeReviewPolicy{PolicyID: hubTestPolicy().ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{{Name: "test", PrincipalID: principal, WorkflowID: "ci.yml", WorkflowSHA256: policy.Digest([]byte("trusted CI")), Source: "independent", MaxAgeSeconds: 3600}}}
	response := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "rules"}, Policy: rules})
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &rules)
	issue := f.create(t, "work")
	path := f.base + "/work-items/" + string(issue.WorkItemID) + "/changes"
	response = performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Native change", Body: "Proposed work"})
	requireNativeStatus(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	decodeHubResponse(t, response, &change)
	return changeFixture{nativeFixture: f, issue: issue, change: change, path: path + "/" + change.ID, rules: rules}
}

func changeTestInput() tracker.ChangeVersionInput {
	return tracker.ChangeVersionInput{BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), MergeBaseSHA: strings.Repeat("a", 40), Repository: "https://github.com/example/repo", Code: changeTestArtifact("code"), Artifacts: []tracker.ChangeArtifact{changeTestArtifact("manifest")}, PolicyID: hubTestPolicy().ID}
}

func changeTestArtifact(kind string) tracker.ChangeArtifact {
	return tracker.ChangeArtifact{Kind: kind, URI: "s3://customer/change/" + kind, SHA256: policy.Digest([]byte(kind)), Availability: "available"}
}

func (f changeFixture) publish(t *testing.T, key, expected string) tracker.ChangeVersion {
	t.Helper()
	input := changeTestInput()
	if expected != "" {
		input.HeadSHA = strings.Repeat("c", 40)
	}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: key}, ExpectedVersionID: expected, ChangeVersionInput: input})
	requireNativeStatus(t, response, http.StatusOK)
	var version tracker.ChangeVersion
	decodeHubResponse(t, response, &version)
	return version
}

func (f changeFixture) detail(t *testing.T) tracker.ChangeDetail {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.path, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var detail tracker.ChangeDetail
	decodeHubResponse(t, response, &detail)
	return detail
}

func changeTestResult(version tracker.ChangeVersion) tracker.SubmitChangeCheck {
	check := version.Checks[0]
	return tracker.SubmitChangeCheck{Mutation: tracker.Mutation{IdempotencyKey: "check"}, ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: check.CheckRunID, HeadSHA: version.HeadSHA, RunID: version.RunID, PolicyID: version.PolicyID, ConfigDigest: version.Policy.ConfigDigest, WorkflowID: check.WorkflowID, WorkflowSHA256: check.WorkflowSHA256, Source: check.Source, Conclusion: "success", CompletedAt: version.CreatedAt, Evidence: []tracker.ChangeArtifact{changeTestArtifact("test")}}}
}

func TestChangeVersionsAndEvidenceSurviveRestart(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	config := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}
	f := newChangeFixture(t, openTestService(t, config))
	if detail := f.detail(t); detail.Summary.Status != "draft" || len(detail.Versions) != 0 {
		t.Fatalf("draft = %#v", detail)
	}
	first := f.publish(t, "v1", "")
	if duplicate := f.publish(t, "v1", ""); duplicate.ID != first.ID {
		t.Fatal("retry changed immutable identity")
	}
	path := f.path + "/versions/" + first.ID
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "approval"}, Decision: "approved"}), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/checks", f.token, changeTestResult(first)), http.StatusOK)
	if summary := f.detail(t).Summary; summary.NativeReview != "approved" || summary.Checks != "success" || summary.Status != "reviewed" || summary.ExternalReview != "not_linked" {
		t.Fatalf("reviewed summary = %#v", summary)
	}
	now = now.Add(time.Hour)
	if summary := f.detail(t).Summary; summary.Checks != "stale" || summary.Status == "reviewed" {
		t.Fatalf("expired evidence = %#v", summary)
	}
	second := f.publish(t, "v2", first.ID)
	for _, test := range []struct {
		name     string
		expected string
		status   int
	}{
		{"stale expected current", first.ID, http.StatusConflict},
		{"missing expected current", "", http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: test.name}, ExpectedVersionID: test.expected, ChangeVersionInput: changeTestInput()}), test.status)
		})
	}
	late := changeTestResult(first)
	late.IdempotencyKey = "late-replay"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/checks", f.token, late), http.StatusOK)
	for _, statement := range []string{"UPDATE change_versions SET number = 10", "DELETE FROM change_versions", "UPDATE change_evidence SET kind = 'review'", "DELETE FROM change_evidence"} {
		if _, err := f.service.database.db.ExecContext(t.Context(), statement); err == nil {
			t.Fatalf("immutable storage accepted %s", statement)
		}
	}
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = openTestService(t, config)
	detail := f.detail(t)
	if detail.Change.ID != f.change.ID || detail.Change.CurrentVersion != second.ID || len(detail.Versions) != 2 || detail.Summary.NativeReview != "stale" || detail.Summary.Checks != "missing" || len(detail.Checks) != 1 {
		t.Fatalf("restarted version history = %#v", detail)
	}
}

func TestChangeCIRejectsForgedAndReplayedResults(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t, nil)
	version := f.publish(t, "v1", "")
	path := f.path + "/versions/" + version.ID
	worker := f.worker(t, "untrusted")
	for _, test := range []struct {
		name string
		edit func(*tracker.SubmitChangeCheck)
	}{
		{"head", func(r *tracker.SubmitChangeCheck) { r.HeadSHA = strings.Repeat("f", 40) }},
		{"run", func(r *tracker.SubmitChangeCheck) { r.RunID = newNativeID("run") }},
		{"check run", func(r *tracker.SubmitChangeCheck) { r.CheckRunID = newNativeID("check") }},
		{"policy", func(r *tracker.SubmitChangeCheck) { r.PolicyID = "other" }},
		{"config", func(r *tracker.SubmitChangeCheck) { r.ConfigDigest = policy.Digest([]byte("forged")) }},
		{"workflow", func(r *tracker.SubmitChangeCheck) { r.WorkflowID = "other.yml" }},
		{"workflow contents", func(r *tracker.SubmitChangeCheck) { r.WorkflowSHA256 = policy.Digest([]byte("forged")) }},
		{"source", func(r *tracker.SubmitChangeCheck) { r.Source = "customer" }},
		{"no evidence", func(r *tracker.SubmitChangeCheck) { r.Evidence = nil }},
		{"raw source", func(r *tracker.SubmitChangeCheck) { r.Evidence[0].URI = "data:text/plain,source" }},
		{"incomplete", func(r *tracker.SubmitChangeCheck) { r.Conclusion = "pending" }},
		{"old completion", func(r *tracker.SubmitChangeCheck) { r.CompletedAt = version.CreatedAt.Add(-time.Second) }},
		{"future completion", func(r *tracker.SubmitChangeCheck) { r.CompletedAt = time.Now().Add(time.Hour) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := changeTestResult(version)
			request.IdempotencyKey = test.name
			test.edit(&request)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/checks", f.token, request), http.StatusUnprocessableEntity)
		})
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/checks", worker, changeTestResult(version)), http.StatusUnprocessableEntity)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/reviews", worker, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "self-review"}, Decision: "approved"}), http.StatusForbidden)
	request := changeTestResult(version)
	request.Conclusion = "failure"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/checks", f.token, request), http.StatusOK)
	request.IdempotencyKey = "alter-terminal"
	request.Conclusion = "success"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/checks", f.token, request), http.StatusConflict)
	request.IdempotencyKey = "check"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/checks", f.token, request), http.StatusConflict)
	if summary := f.detail(t).Summary; summary.Checks != "failure" {
		t.Fatalf("failed evidence was upgraded: %#v", summary)
	}
}

func TestChangeDiscussionLinksAndProjectIsolation(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t, nil)
	other := newNativeFixture(t, f.service, "", "other")
	linked := f.create(t, "linked")
	create := tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "linked-change"}, Title: "Linked change", LinkedIssues: []tracker.NativeWorkItemID{linked.WorkItemID}}
	response := performHubAPIRequest(t, f.service, http.MethodPost, strings.TrimSuffix(f.path, "/"+f.change.ID), f.token, create)
	requireNativeStatus(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	decodeHubResponse(t, response, &change)
	response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(linked.WorkItemID)+"/changes", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var changes []tracker.ChangeRequest
	decodeHubResponse(t, response, &changes)
	if len(changes) != 1 || changes[0].ID != change.ID {
		t.Fatalf("linked changes = %#v", changes)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, f.path, other.token, nil), http.StatusNotFound)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", other.token, tracker.PublishChangeVersion{}), http.StatusNotFound)
	version := f.publish(t, "v1", "")
	now := time.Now().UTC().Add(-time.Minute)
	for _, imported := range []bool{false, true} {
		request := tracker.DiscussChange{Mutation: tracker.Mutation{IdempotencyKey: "native"}, VersionID: version.ID, Body: "Native discussion <script>"}
		if imported {
			request.IdempotencyKey, request.Body = "import", "Original GitHub discussion"
			request.Provenance = &tracker.Provenance{Provider: "github", ExternalID: "comment-123", AuthorID: "reviewer", CreatedAt: now, UpdatedAt: now, ObservedAt: now}
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/discussion", f.token, request), http.StatusOK)
		if imported {
			request.IdempotencyKey = "reimport"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/discussion", f.token, request), http.StatusOK)
		}
	}
	detail := f.detail(t)
	if len(detail.Discussion) != 2 || detail.Discussion[0].Provenance != nil || detail.Discussion[1].Provenance.AuthorID != "reviewer" {
		t.Fatalf("discussion provenance = %#v", detail.Discussion)
	}
}

func TestChangeVersionConcurrentPublication(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t, nil)
	var group sync.WaitGroup
	statuses := make(chan int, 2)
	for _, key := range []string{"first", "second"} {
		group.Go(func() {
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: key}, ChangeVersionInput: changeTestInput()})
			statuses <- response.Code
		})
	}
	group.Wait()
	close(statuses)
	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[http.StatusOK] != 1 || counts[http.StatusConflict] != 1 || len(f.detail(t).Versions) != 1 {
		t.Fatalf("concurrent publish statuses = %v", counts)
	}
}

func TestChangeWorkerRequiresCurrentAttempt(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t, nil)
	worker := f.worker(t, "worker")
	lease := claimNativeAttempt(t, f.nativeFixture, worker, "machine", "session", f.issue.WorkItemID)
	event := nativeStartedEvent(lease)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", worker, event), http.StatusOK)
	for _, test := range []struct {
		name string
		edit func(*tracker.PublishChangeVersion)
		want int
	}{
		{"missing fence", func(r *tracker.PublishChangeVersion) { r.LeaseID = "" }, http.StatusConflict},
		{"stale fence", func(r *tracker.PublishChangeVersion) { r.FencingToken++ }, http.StatusConflict},
		{"unrelated run", func(r *tracker.PublishChangeVersion) { r.RunID = newNativeID("run") }, http.StatusNotFound},
		{"missing run", func(r *tracker.PublishChangeVersion) { r.RunID, r.AttemptID = "", "" }, http.StatusUnprocessableEntity},
		{"current", func(_ *tracker.PublishChangeVersion) {}, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: test.name, LeaseID: lease.ID, FencingToken: lease.FencingToken}, ChangeVersionInput: changeTestInput()}
			request.RunID, request.AttemptID = event.Data.RunID, event.Data.AttemptID
			test.edit(&request)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", worker, request), test.want)
		})
	}
	data, err := json.Marshal(f.detail(t))
	if err != nil || !strings.Contains(string(data), event.Data.AttemptID) {
		t.Fatalf("version lost linked run: %s, %v", data, err)
	}
}

func TestChangeInvalidVersionAndReviewPolicy(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t, nil)
	for _, test := range []struct {
		name string
		edit func(*tracker.ChangeVersionInput)
		want int
	}{
		{"base", func(v *tracker.ChangeVersionInput) { v.BaseSHA = "main" }, http.StatusUnprocessableEntity},
		{"head", func(v *tracker.ChangeVersionInput) { v.HeadSHA = "HEAD" }, http.StatusUnprocessableEntity},
		{"merge base", func(v *tracker.ChangeVersionInput) { v.MergeBaseSHA = "" }, http.StatusUnprocessableEntity},
		{"repository", func(v *tracker.ChangeVersionInput) { v.Repository = "file:///source" }, http.StatusUnprocessableEntity},
		{"code hash", func(v *tracker.ChangeVersionInput) { v.Code.SHA256 = "missing" }, http.StatusUnprocessableEntity},
		{"policy", func(v *tracker.ChangeVersionInput) { v.PolicyID = "other" }, http.StatusConflict},
		{"unpaired attempt", func(v *tracker.ChangeVersionInput) { v.RunID = newNativeID("run") }, http.StatusUnprocessableEntity},
		{"forged PR identity", func(v *tracker.ChangeVersionInput) {
			v.External = &tracker.ChangeExternalReference{Provider: "github", ID: "2", URL: "https://github.com/example/repo/pull/1"}
		}, http.StatusUnprocessableEntity},
		{"unsafe PR URL", func(v *tracker.ChangeVersionInput) {
			v.External = &tracker.ChangeExternalReference{Provider: "github", ID: "1", URL: "javascript:alert(1)"}
		}, http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := changeTestInput()
			test.edit(&input)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: test.name}, ChangeVersionInput: input}), test.want)
		})
	}
	for _, token := range []string{f.token, f.worker(t, "worker")} {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", token, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "forge-policy"}, Policy: f.rules}), http.StatusForbidden)
	}
	request := tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "stale-policy"}, Policy: f.rules}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, request), http.StatusConflict)
	request.IdempotencyKey, request.ExpectedID = "unknown-ci", f.rules.ID
	request.Policy.RequiredChecks[0].PrincipalID = "unknown"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, request), http.StatusUnprocessableEntity)
}

func TestChangeApprovalPreservesProtectedMerge(t *testing.T) {
	t.Parallel()
	backend := &recordingOutboxBackend{verifyFailures: []error{Permanent(errors.New("required GitHub review and fresh checks unavailable"))}}
	service := openManualOutboxService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")}, backend)
	f := newChangeFixture(t, service)
	repositoryID, _ := seedProjection(t, service.database.db)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{"UPDATE projects SET repository_id = NULL WHERE repository_id = ?", []any{repositoryID}},
		{"UPDATE projects SET repository_id = ?, github_repository_enabled = 1 WHERE id = ?", []any{repositoryID, f.project.ID}},
		{"UPDATE issues SET repository_id = ? WHERE native_id = ?", []any{repositoryID, f.issue.WorkItemID}},
		{"UPDATE pull_requests SET issue_id = (SELECT id FROM issues WHERE native_id = ?), url = 'https://github.com/example/repo/pull/1', head_sha = ?, reviews_summary_json = ?", []any{f.issue.WorkItemID, strings.Repeat("b", 40), `{"decision":"changes_requested"}`}},
	} {
		if _, err := service.database.db.ExecContext(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	input := changeTestInput()
	input.External = &tracker.ChangeExternalReference{Provider: "github", ID: "1", URL: "https://github.com/example/repo/pull/1"}
	response := performHubAPIRequest(t, service, http.MethodPost, f.path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "external-version"}, ChangeVersionInput: input})
	requireNativeStatus(t, response, http.StatusOK)
	var version tracker.ChangeVersion
	decodeHubResponse(t, response, &version)
	path := f.path + "/versions/" + version.ID
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "native-approval"}, Decision: "approved"}), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path+"/checks", f.token, changeTestResult(version)), http.StatusOK)
	detail := f.detail(t)
	if detail.Summary.NativeReview != "approved" || detail.Summary.ExternalReview != "snapshot: changes_requested" || detail.External == nil || detail.External.Merge.Ready {
		t.Fatalf("native and external review authority = %#v", detail)
	}
	var issueID int64
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE native_id = ?", f.issue.WorkItemID).Scan(&issueID); err != nil {
		t.Fatal(err)
	}
	_, err := service.AppendWorkEvent(t.Context(), WorkEventChange{IssueID: issueID, Kind: "merge_requested", Mutation: MergePullRequestMutation{IdempotencyKey: "protected-merge", RepositoryID: repositoryID, IssueID: issueID, PullRequestNumber: 1, HeadSHA: version.HeadSHA, MergeMethod: "squash"}})
	if err != nil {
		t.Fatal(err)
	}
	processed, err := service.ProcessOutbox(t.Context())
	if !processed || err == nil || backend.verifyCalls != 1 || backend.executeCalls != 0 || backend.effects != 0 {
		t.Fatalf("protected merge bypassed: processed=%v error=%v backend=%#v", processed, err, backend)
	}
	if _, err := service.database.db.ExecContext(t.Context(), "UPDATE pull_requests SET head_sha = ?", strings.Repeat("c", 40)); err != nil {
		t.Fatal(err)
	}
	if got := f.detail(t).Summary.ExternalReview; got != "stale_head" {
		t.Fatalf("moved external head review = %s", got)
	}
}
