package hubserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
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

func (f changeFixture) publish(t *testing.T, key, expected string, retained ...bool) tracker.ChangeVersion {
	t.Helper()
	input := changeTestInput()
	if expected != "" {
		input.HeadSHA = strings.Repeat("c", 40)
	}
	var bundle []byte
	if len(retained) > 0 && retained[0] {
		bundle = []byte("retained admission fixture")
		input.Source = &tracker.ChangeSource{Format: "git-bundle", BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA, BundleSHA256: tracker.ChangeSourceDigest(bundle), DiffSHA256: strings.Repeat("d", 64), Bytes: int64(len(bundle))}
	}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: key}, ExpectedVersionID: expected, ChangeVersionInput: input, SourceBundle: bundle})
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
	current, err := readCurrentChangeDetail(t.Context(), f.service.database.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, detail.Change, f.service.config.now())
	if err != nil || !reflect.DeepEqual(current.Summary, detail.Summary) {
		t.Fatalf("current summary = %#v, full summary = %#v, error = %v", current.Summary, detail.Summary, err)
	}
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
	snapshot := linkedSnapshot()
	snapshot.URL = "https://github.com/digitaldrywood/detent/issues/12"
	f.service.config.ImportBackend = linkedTestImporter{snapshot: snapshot}
	repositoryID, _ := seedProjection(t, f.service.database.db)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{"UPDATE projects SET repository_id = NULL WHERE repository_id = ?", []any{repositoryID}},
		{"UPDATE projects SET repository_id = ? WHERE id = ?", []any{repositoryID, f.project.ID}},
		{"UPDATE issues SET repository_id = ?, github_node_id = 'I_original', github_number = 3410, url = 'https://github.com/wrong/repo/issues/29', source_updated_at = ?, synchronized_at = ? WHERE native_id = ?", []any{repositoryID, testTimestamp, testTimestamp, f.issue.WorkItemID}},
	} {
		if _, err := f.service.database.db.ExecContext(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	migrated := readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, "")
	wantImported := tracker.GitHubIssueSourceReference("I_original", "https://github.com/digitaldrywood/detent/issues/3410")
	if migrated.Number == 3410 || !reflect.DeepEqual(migrated.ExternalReferences, []tracker.ExternalReference{wantImported}) {
		t.Fatalf("migration lost source identity: %+v", migrated)
	}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "linked-source"}, GitHubIssueURL: snapshot.URL, Title: "linked", State: f.issue.State})
	requireNativeStatus(t, response, http.StatusOK)
	var linked tracker.NativeIssue
	decodeHubResponse(t, response, &linked)
	create := tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "linked-change"}, Title: "Linked change", LinkedIssues: []tracker.NativeWorkItemID{linked.WorkItemID}}
	response = performHubAPIRequest(t, f.service, http.MethodPost, strings.TrimSuffix(f.path, "/"+f.change.ID), f.token, create)
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
	response = performHubAPIRequest(t, f.service, http.MethodGet, strings.TrimSuffix(f.path, "/"+f.change.ID)+"/"+change.ID, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var delivery tracker.ChangeDetail
	decodeHubResponse(t, response, &delivery)
	if len(delivery.SourceIssues) != 2 || tracker.AppendGitHubIssueClosingReferences("Delivery", delivery.SourceIssues) != "Delivery\n\nCloses digitaldrywood/detent#12\nCloses digitaldrywood/detent#3410" {
		t.Fatalf("delivered issue sources = %+v", delivery.SourceIssues)
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
		{"changed source bytes", func(r *tracker.PublishChangeVersion) { r.SourceBundle = []byte("other source") }, http.StatusUnprocessableEntity},
		{"wrong source base", func(r *tracker.PublishChangeVersion) { r.Source.BaseSHA = r.HeadSHA }, http.StatusUnprocessableEntity},
		{"wrong source head", func(r *tracker.PublishChangeVersion) { r.Source.HeadSHA = r.BaseSHA }, http.StatusUnprocessableEntity},
		{"missing source metadata", func(r *tracker.PublishChangeVersion) { r.Source = nil }, http.StatusUnprocessableEntity},
		{"current", func(_ *tracker.PublishChangeVersion) {}, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: test.name, LeaseID: lease.ID, FencingToken: lease.FencingToken}, ChangeVersionInput: changeTestInput()}
			request.RunID, request.AttemptID = event.Data.RunID, event.Data.AttemptID
			request.SourceBundle = []byte("retained source")
			request.Source = &tracker.ChangeSource{Format: "git-bundle", BaseSHA: request.BaseSHA, HeadSHA: request.HeadSHA, BundleSHA256: tracker.ChangeSourceDigest(request.SourceBundle), DiffSHA256: strings.Repeat("d", 64), Bytes: int64(len(request.SourceBundle))}
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

// TestChangeLanding checks what recording a landing means: only a reviewed
// current version lands, the landing finishes the primary issue in a
// terminal lane in the same transaction, the change reports itself landed,
// and nothing further is reviewed or landed on it.
func TestChangeLanding(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 29, 1, 0, 0, 0, time.UTC)
	f := newChangeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}))
	first := f.publish(t, "v1", "")
	landing := func(key, version string, request tracker.LandChangeVersion) *httptest.ResponseRecorder {
		request.IdempotencyKey = key
		return performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions/"+version+"/landing", f.token, request)
	}
	land := tracker.LandChangeVersion{MergeSHA: strings.Repeat("e", 40), BaseRef: "main", Method: "squash"}
	requireNativeStatus(t, landing("unreviewed", first.ID, land), http.StatusConflict)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions/"+first.ID+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "approval"}, Decision: "approved"}), http.StatusOK)
	requireNativeStatus(t, landing("unchecked", first.ID, land), http.StatusConflict)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions/"+first.ID+"/checks", f.token, changeTestResult(first)), http.StatusOK)
	updated := hubTestPolicy()
	updated.ConfigDigest = policy.Digest([]byte("non-gate configuration after checks completed"))
	updated = updated.WithID()
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: first.PolicyID, Policy: updated}), http.StatusOK)
	if detail := f.detail(t); len(detail.Versions) != 1 || detail.Versions[0].PolicyID != first.PolicyID || detail.Versions[0].ReviewPolicy.ID != first.ReviewPolicy.ID || len(detail.Checks) != 1 || detail.Checks[0].PolicyID != first.PolicyID || detail.Checks[0].ConfigDigest != first.Policy.ConfigDigest {
		t.Fatalf("policy approval changed immutable acceptance evidence: %+v", detail)
	}
	if summary := f.detail(t).Summary; summary.Status != "reviewed" {
		t.Fatalf("summary before landing = %#v", summary)
	}
	for _, test := range []struct {
		name    string
		request tracker.LandChangeVersion
	}{
		{"uppercase merge sha", tracker.LandChangeVersion{MergeSHA: strings.Repeat("E", 40), BaseRef: "main", Method: "squash"}},
		{"blank base", tracker.LandChangeVersion{MergeSHA: strings.Repeat("e", 40), BaseRef: " ", Method: "squash"}},
		{"unknown method", tracker.LandChangeVersion{MergeSHA: strings.Repeat("e", 40), BaseRef: "main", Method: "octopus"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireNativeStatus(t, landing(test.name, first.ID, test.request), http.StatusUnprocessableEntity)
		})
	}
	response := landing("land", first.ID, land)
	requireNativeStatus(t, response, http.StatusOK)
	var landed tracker.ChangeRequest
	decodeHubResponse(t, response, &landed)
	if landed.Landed == nil || landed.Landed.VersionID != first.ID || landed.Landed.MergeSHA != land.MergeSHA || landed.Landed.BaseRef != "main" || landed.Landed.Method != "squash" || landed.Landed.HeadSHA != first.HeadSHA || !landed.Landed.LandedAt.Equal(now) {
		t.Fatalf("landed change = %#v", landed.Landed)
	}

	if landed.Landed.Quality == nil || landed.Landed.Quality.IssueBody != f.issue.Body || landed.Landed.Quality.IssueRevision != f.issue.Revision {
		t.Fatalf("landing must freeze the contract: %+v", landed.Landed.Quality)
	}
	stored, err := readQualityLanding(t.Context(), f.service.database.db, landed, first.ID)
	if err != nil || stored == nil || stored.Quality == nil || stored.Quality.IssueBody != landed.Landed.Quality.IssueBody {
		t.Fatalf("persistent landing context = %+v, %v", stored, err)
	}

	projected, _, err := readNativeIssueProjection(t.Context(), f.service.database.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, string(f.issue.WorkItemID), true)
	if err != nil {
		t.Fatal(err)
	}
	if projected.ListChange == nil || projected.ListChange.Branch != land.BaseRef || projected.ListChange.HeadSHA != land.MergeSHA {
		t.Fatalf("list change = %#v", projected.ListChange)
	}

	for _, test := range []struct {
		name     string
		scope    nativeScope
		from, to time.Time
		want     int
	}{
		{"landed version", nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, now.Add(-time.Hour), now.Add(time.Hour), 1},
		{"exclusive end", nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, now.Add(-time.Hour), now, 0},
		{"outside window", nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, now.Add(time.Second), now.Add(time.Hour), 0},
		{"foreign project", nativeScope{organization: f.project.OrganizationID, project: "prj_foreign"}, now.Add(-time.Hour), now.Add(time.Hour), 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := readNativeAnalytics(t.Context(), f.service.database.db, test.scope, operatortool.AnalyticsRequest{Limit: 1}, operatortool.AnalyticsWindow{From: test.from, To: test.to, Bucket: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			if got.CostPerOutcome.Shipped != test.want || len(got.Landings.Items) != test.want {
				t.Fatalf("shipping evidence %#v", got)
			}
			if test.want == 1 && (got.Landings.Items[0].Landing.MergeSHA != land.MergeSHA || got.Landings.Items[0].Landing.VersionID != first.ID || got.Digest[1].Shipped != 1) {
				t.Fatalf("lost exact-version provenance %#v", got)
			}
		})
	}
	replay := landing("land", first.ID, land)
	requireNativeStatus(t, replay, http.StatusOK)
	if replay.Body.String() != response.Body.String() {
		t.Fatal("replaying the landing changed the record")
	}
	requireNativeStatus(t, landing("again", first.ID, tracker.LandChangeVersion{MergeSHA: strings.Repeat("f", 40), BaseRef: "main", Method: "squash"}), http.StatusConflict)
	detail := f.detail(t)
	if detail.Summary.Status != "landed" || detail.Summary.NativeReview != "approved" || len(detail.Summary.Messages) != 1 || !strings.Contains(detail.Summary.Messages[0], land.MergeSHA) {
		t.Fatalf("landed summary = %#v", detail.Summary)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions/"+first.ID+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "late-review"}, Decision: "changes_requested", Body: "too late"}), http.StatusConflict)
	response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(f.issue.WorkItemID), f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	if !issue.Terminal {
		t.Fatalf("the landed item is in %s, not a terminal lane", issue.State)
	}
}

// TestApprovalMovesToLandingLane checks the move an approval implies: a
// reviewed current version moves the primary issue to the Merging lane when
// the workflow has one reachable from the issue's lane, and only then.
func TestApprovalMovesToLandingLane(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		states []tracker.NativeState
		from   string
		want   string
	}{
		{name: "hosted template lands", states: HostedProjectStates(), from: "Human Review", want: "Merging"},
		{name: "no landing lane stays", states: []tracker.NativeState{
			{Name: "Todo", Dispatchable: true, Transitions: []string{"Review"}},
			{Name: "Review", Transitions: []string{"Done"}},
			{Name: "Done", Terminal: true},
		}, from: "Review", want: "Review"},
		{name: "landing lane not reachable stays", states: []tracker.NativeState{
			{Name: "Todo", Dispatchable: true, Transitions: []string{"Review", "Merging"}},
			{Name: "Review", Transitions: []string{"Done"}},
			{Name: "Merging", Dispatchable: true, Transitions: []string{"Done"}},
			{Name: "Done", Terminal: true},
		}, from: "Review", want: "Review"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "landing")
			if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM workflow_states WHERE project_id = ?", f.project.ID); err != nil {
				t.Fatal(err)
			}
			raw, err := marshalNative(test.states)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET states_json = ? WHERE id = ?", raw, f.project.ID); err != nil {
				t.Fatal(err)
			}
			for _, state := range test.states {
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states (project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)", f.project.ID, state.Name, state.Name, state.Terminal, state.Dispatchable, testTimestamp, testTimestamp); err != nil {
					t.Fatal(err)
				}
			}
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "work"}, Title: "Land me", State: test.from})
			requireNativeStatus(t, response, http.StatusOK)
			var issue tracker.NativeIssue
			decodeHubResponse(t, response, &issue)
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/changes"
			response = performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Native change"})
			requireNativeStatus(t, response, http.StatusOK)
			var change tracker.ChangeRequest
			decodeHubResponse(t, response, &change)
			path += "/" + change.ID
			cf := changeFixture{nativeFixture: f, issue: issue, change: change, path: path}
			version := cf.publish(t, "publish", "", true)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "approve"}, Decision: "approved"}), http.StatusOK)
			response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID), f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &issue)
			if issue.State != test.want {
				t.Fatalf("after approval the item is in %s, want %s", issue.State, test.want)
			}
			if test.want == "Merging" {
				r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
				r.enroll(t)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, map[string]any{"expected_revision": 1, "display_name": "Landing runner", "state": "active", "capacity_limit": 0, "project_ids": []tracker.ProjectID{f.project.ID}}), http.StatusOK)
				runtimePath := f.base + "/work-items/" + string(issue.WorkItemID) + "/runtime"
				response = performHubAPIRequest(t, f.service, http.MethodGet, runtimePath, f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var evidence tracker.NativeRuntimeEvidence
				decodeHubResponse(t, response, &evidence)
				if evidence.Attempt != nil || evidence.LatestDecision != nil || evidence.Scheduling.Outcome != "skipped" || len(evidence.Capacity) != 1 || evidence.Capacity[0].Available != 0 || evidence.Change.Change.CurrentVersion != version.ID || len(evidence.Capacity[0].Exclusions) == 0 {
					t.Fatalf("unclaimed landing evidence=%#v", evidence)
				}
				claim := tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "capacity-refusal", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
				assertUnchangedDecision := func() {
					t.Helper()
					// Finish credential bookkeeping before measuring decision writes.
					f.service.tokenUseWork.Wait()
					var before, after int64
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT total_changes()").Scan(&before); err != nil {
						t.Fatal(err)
					}
					for range 16 {
						requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), http.StatusConflict)
					}
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT total_changes()").Scan(&after); err != nil || after != before {
						t.Fatalf("unchanged decisions wrote %d rows, err=%v", after-before, err)
					}
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), http.StatusConflict)
				assertUnchangedDecision()
				decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, runtimePath, f.token, nil), &evidence)
				if evidence.Attempt != nil || evidence.LatestDecision == nil || evidence.LatestDecision.Actor.Kind != "runner" || evidence.LatestDecision.Data.Decision.Source != "native_runner_routing" || evidence.LatestDecision.Data.Decision.Outcome != "skipped" || evidence.LatestDecision.Data.Decision.RunnerID != r.binding.RunnerID || evidence.LatestDecision.Data.Decision.WorkItemRevision != issue.Revision {
					t.Fatalf("recorded routing refusal=%#v", evidence)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE machines SET capacity=0 WHERE id=?", r.binding.MachineID); err != nil {
					t.Fatal(err)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), http.StatusConflict)
				assertUnchangedDecision()
				decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, runtimePath, f.token, nil), &evidence)
				if evidence.LatestDecision.Data.Decision.Source != "native_host_capacity" || evidence.Attempt != nil {
					t.Fatalf("recorded host refusal=%#v", evidence)
				}
				previousDecision := evidence.LatestDecision.ID
				if evidence.LatestDecision.Data.Change == nil || evidence.LatestDecision.Data.Change.VersionID != version.ID {
					t.Fatalf("decision lost Change evidence=%#v", evidence.LatestDecision)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET revision=revision+1 WHERE native_id=?", issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), http.StatusConflict)
				assertUnchangedDecision()
				decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, runtimePath, f.token, nil), &evidence)
				if evidence.LatestDecision.ID == previousDecision || evidence.LatestDecision.Data.Decision.WorkItemRevision != issue.Revision+1 {
					t.Fatalf("changed revision lost decision=%#v", evidence.LatestDecision)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE machines SET capacity=2 WHERE id=?", r.binding.MachineID); err != nil {
					t.Fatal(err)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, map[string]any{"expected_revision": 2, "display_name": "Landing runner", "state": "active", "capacity_limit": 2, "project_ids": []tracker.ProjectID{f.project.ID}}), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), http.StatusOK)
				var evaluatedReady int
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE work_item_id=? AND type='scheduler.decision' AND json_extract(data_json, '$.decision.source')='native_claim_eligibility' AND json_extract(data_json, '$.decision.outcome')='ready' AND json_extract(data_json, '$.change.version_id')=?", issue.WorkItemID, version.ID).Scan(&evaluatedReady); err != nil || evaluatedReady != 1 {
					t.Fatalf("actual reviewed Change evaluation missing: count=%d error=%v", evaluatedReady, err)
				}
			}
		})
	}
}

// TestReviewedChangePromotesOnAnyEvidence checks that the move to the landing
// lane follows whichever write completes the evidence: an approval given
// before the check, a check reported after the approval, and a publish under
// a policy that requires no review.
func TestReviewedChangePromotesOnAnyEvidence(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		require bool
		order   []string
	}{
		{name: "check after approval", require: true, order: []string{"approve", "check"}},
		{name: "approval after check", require: true, order: []string{"check", "approve"}},
		{name: "no review required", require: false, order: []string{"check"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "promote")
			if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM workflow_states WHERE project_id = ?", f.project.ID); err != nil {
				t.Fatal(err)
			}
			states := HostedProjectStates()
			raw, err := marshalNative(states)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET states_json = ? WHERE id = ?", raw, f.project.ID); err != nil {
				t.Fatal(err)
			}
			for _, state := range states {
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states (project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)", f.project.ID, state.Name, state.Name, state.Terminal, state.Dispatchable, testTimestamp, testTimestamp); err != nil {
					t.Fatal(err)
				}
			}
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			var principal string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT id FROM api_tokens WHERE name = 'operator-promote'").Scan(&principal); err != nil {
				t.Fatal(err)
			}
			rules := tracker.ChangeReviewPolicy{PolicyID: hubTestPolicy().ID, RequireReview: test.require, RequiredChecks: []tracker.ChangeCheckSpec{{Name: "test", PrincipalID: principal, WorkflowID: "ci.yml", WorkflowSHA256: policy.Digest([]byte("trusted CI")), Source: "independent", MaxAgeSeconds: 3600}}}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "rules"}, Policy: rules}), http.StatusOK)
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "work"}, Title: "Promote me", State: "Human Review"})
			requireNativeStatus(t, response, http.StatusOK)
			var issue tracker.NativeIssue
			decodeHubResponse(t, response, &issue)
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/changes"
			response = performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Native change"})
			requireNativeStatus(t, response, http.StatusOK)
			var change tracker.ChangeRequest
			decodeHubResponse(t, response, &change)
			path += "/" + change.ID
			response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "publish"}, ChangeVersionInput: changeTestInput()})
			requireNativeStatus(t, response, http.StatusOK)
			var version tracker.ChangeVersion
			decodeHubResponse(t, response, &version)
			state := func() string {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID), f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var current tracker.NativeIssue
				decodeHubResponse(t, response, &current)
				return current.State
			}
			if got := state(); got != "Human Review" {
				t.Fatalf("before evidence the item is in %s", got)
			}
			for i, step := range test.order {
				switch step {
				case "approve":
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "approve"}, Decision: "approved"}), http.StatusOK)
				case "check":
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/checks", f.token, changeTestResult(version)), http.StatusOK)
				}
				want := "Human Review"
				if i == len(test.order)-1 {
					want = "Merging"
				}
				if got := state(); got != want {
					t.Fatalf("after %s the item is in %s, want %s", step, got, want)
				}
			}
		})
	}
}

// TestRequestChangesReturnsToWork checks what a request for changes means for
// the primary issue: the review text lands on the issue as the reviewer's
// comment, and an issue waiting in review moves back to a working lane, while
// an issue already being worked stays put and a finished one is untouched.
func TestRequestChangesReturnsToWork(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		from        string
		want        string
		wantComment bool
	}{
		{name: "review returns to In Progress", from: "Human Review", want: "In Progress", wantComment: true},
		{name: "landing lane returns to In Progress", from: "Merging", want: "In Progress", wantComment: true},
		{name: "a working item stays where it is", from: "In Progress", want: "In Progress", wantComment: true},
		{name: "a finished item is untouched", from: "Done", want: "Done", wantComment: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "rework")
			if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM workflow_states WHERE project_id = ?", f.project.ID); err != nil {
				t.Fatal(err)
			}
			states := HostedProjectStates()
			raw, err := marshalNative(states)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET states_json = ? WHERE id = ?", raw, f.project.ID); err != nil {
				t.Fatal(err)
			}
			for _, state := range states {
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states (project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)", f.project.ID, state.Name, state.Name, state.Terminal, state.Dispatchable, testTimestamp, testTimestamp); err != nil {
					t.Fatal(err)
				}
			}
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "work"}, Title: "Rework me", State: test.from})
			requireNativeStatus(t, response, http.StatusOK)
			var issue tracker.NativeIssue
			decodeHubResponse(t, response, &issue)
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/changes"
			response = performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Native change"})
			requireNativeStatus(t, response, http.StatusOK)
			var change tracker.ChangeRequest
			decodeHubResponse(t, response, &change)
			path += "/" + change.ID
			response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "publish"}, ChangeVersionInput: changeTestInput()})
			requireNativeStatus(t, response, http.StatusOK)
			var version tracker.ChangeVersion
			decodeHubResponse(t, response, &version)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "rework"}, Decision: "changes_requested", Body: "Move the link to the end of the row."}), http.StatusOK)
			response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID), f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &issue)
			if issue.State != test.want {
				t.Fatalf("after the request the item is in %s, want %s", issue.State, test.want)
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID)+"/comments", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var comments tracker.Page[tracker.NativeComment]
			decodeHubResponse(t, response, &comments)
			found := false
			for _, comment := range comments.Items {
				if strings.Contains(comment.Body, "Changes requested on Change Request "+change.ID) && strings.Contains(comment.Body, "Move the link to the end of the row.") && comment.Actor.Kind == "human" {
					found = true
				}
			}
			if found != test.wantComment {
				t.Fatalf("instruction comment present = %t, want %t: %#v", found, test.wantComment, comments.Items)
			}
		})
	}
}

// TestRequestChangesOnAnOlderVersionMovesNothing checks that a late decision
// on a superseded version is recorded on the change and leaves the issue
// where the current version put it.
func TestRequestChangesOnAnOlderVersionMovesNothing(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t, nil)
	first := f.publish(t, "v1", "")
	f.publish(t, "v2", first.ID)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions/"+first.ID+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "late"}, Decision: "changes_requested", Body: "About round one."}), http.StatusOK)
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/comments", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var comments tracker.Page[tracker.NativeComment]
	decodeHubResponse(t, response, &comments)
	for _, comment := range comments.Items {
		if strings.Contains(comment.Body, "About round one.") {
			t.Fatalf("a decision on an older version reached the issue: %#v", comment)
		}
	}
	if len(f.detail(t).Reviews) != 1 {
		t.Fatal("the late review was not recorded on the change")
	}
}

func TestReviewInstructionsStayWithinTheCommentLimit(t *testing.T) {
	t.Parallel()
	change := tracker.ChangeRequest{ID: "change_1"}
	review := tracker.ChangeReview{VersionID: "version_1", Body: strings.Repeat("é", 40<<10)}
	body := reviewInstructions(change, review)
	if len(body) > maxNativeCommentBytes || !utf8.ValidString(body) || !strings.HasPrefix(body, "Changes requested on Change Request change_1 (version version_1).\n\n") {
		t.Fatalf("instructions = %d bytes, valid = %t", len(body), utf8.ValidString(body))
	}
	if short := reviewInstructions(change, tracker.ChangeReview{Body: " Move it. "}); short != "Changes requested on Change Request change_1.\n\nMove it." {
		t.Fatalf("short instructions = %q", short)
	}
}

// TestPublishUnderTheDefaultPolicyLands checks the default review policy on
// the hosted template: a published version needs nobody, so an item waiting
// in review moves to Merging when it is published, while an item a run is
// still working stays where it is for that run's completion to move.
func TestPublishUnderTheDefaultPolicyLands(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		from string
		want string
	}{
		{from: "Human Review", want: "Merging"},
		{from: "In Progress", want: "In Progress"},
	} {
		t.Run(test.from, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "default")
			if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM workflow_states WHERE project_id = ?", f.project.ID); err != nil {
				t.Fatal(err)
			}
			states := HostedProjectStates()
			raw, err := marshalNative(states)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET states_json = ? WHERE id = ?", raw, f.project.ID); err != nil {
				t.Fatal(err)
			}
			for _, state := range states {
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states (project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)", f.project.ID, state.Name, state.Name, state.Terminal, state.Dispatchable, testTimestamp, testTimestamp); err != nil {
					t.Fatal(err)
				}
			}
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "work"}, Title: "Land me", State: test.from})
			requireNativeStatus(t, response, http.StatusOK)
			var issue tracker.NativeIssue
			decodeHubResponse(t, response, &issue)
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/changes"
			response = performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Native change"})
			requireNativeStatus(t, response, http.StatusOK)
			var change tracker.ChangeRequest
			decodeHubResponse(t, response, &change)
			path += "/" + change.ID
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "publish"}, ChangeVersionInput: changeTestInput()}), http.StatusOK)
			response = performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var detail tracker.ChangeDetail
			decodeHubResponse(t, response, &detail)
			if detail.Summary.Status != "reviewed" || detail.Summary.NativeReview != "not_required" {
				t.Fatalf("summary under the default policy = %#v, want reviewed with no review required", detail.Summary)
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID), f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &issue)
			if issue.State != test.want {
				t.Fatalf("after publishing the item is in %s, want %s", issue.State, test.want)
			}
		})
	}
}
