package hubserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
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

func TestNativeRecoveryRetainsCurrentReviewEvidence(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}
	t.Parallel()
	for _, test := range []struct {
		name                 string
		sourceVerdict        string
		acceptanceVerdict    string
		sourceAfterValidator bool
		ordinaryOnly         bool
		oversizedSource      bool
		oversizedAcceptance  bool
		oversizedHuman       bool
	}{
		{name: "fresh acceptance follows source", sourceVerdict: "pass"},
		{name: "source pass requires newer acceptance", sourceVerdict: "pass", sourceAfterValidator: true},
		{name: "latest source wait is retained", sourceVerdict: "wait", sourceAfterValidator: true},
		{name: "latest source rework is retained", sourceVerdict: "rework", sourceAfterValidator: true},
		{name: "latest acceptance wait is retained", sourceVerdict: "pass", acceptanceVerdict: "wait"},
		{name: "latest acceptance rework is retained", sourceVerdict: "pass", acceptanceVerdict: "rework"},
		{name: "ordinary reviews remain complete", ordinaryOnly: true},
		{name: "single authoritative source remains oversized", sourceVerdict: "pass", oversizedSource: true},
		{name: "single authoritative acceptance remains oversized", sourceVerdict: "pass", oversizedAcceptance: true},
		{name: "ordinary human review remains oversized", sourceVerdict: "pass", oversizedHuman: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newChangeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")}))
			version := f.publish(t, "retained-version", "", true)
			automatic := func(id, verdict string, source bool, bytes int) tracker.ChangeReview {
				result := &gate.ValidatorResult{SessionID: 42, VersionID: version.ID, Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, Submitted: true, Verdict: verdict, Commands: []gate.CommandResult{{Command: "make check-land", Output: strings.Repeat("x", bytes)}}}
				review := tracker.ChangeReview{ID: id, VersionID: version.ID, Decision: "approved", Actor: tracker.Actor{Kind: "runner", PrincipalID: "worker"}, CreatedAt: version.CreatedAt}
				if source {
					review.ImplementationReview = result
				} else {
					review.Validator = result
				}
				switch verdict {
				case "wait":
					review.Decision = "commented"
				case "rework":
					review.Decision = "changes_requested"
				}
				return review
			}
			human := tracker.ChangeReview{ID: "review_human", VersionID: version.ID, Decision: "changes_requested", Body: "Preserve the existing behavior.", Actor: tracker.Actor{Kind: "human", PrincipalID: "reviewer"}, CreatedAt: version.CreatedAt}
			bot := tracker.ChangeReview{ID: "review_codex", VersionID: version.ID, Decision: "commented", Body: "Independent source feedback.", Actor: tracker.Actor{Kind: "bot", PrincipalID: "codex"}, CreatedAt: version.CreatedAt}
			if test.oversizedHuman {
				human.Body = strings.Repeat("h", 1<<20)
			}
			var history, want []tracker.ChangeReview
			if !test.ordinaryOnly {
				for i := range 5 {
					history = append(history, automatic(fmt.Sprintf("review_superseded_%d", i), "pass", i%2 == 0, 150<<10))
					if i == 1 {
						history = append(history, human)
						want = append(want, human)
					}
				}
				bytes := 160 << 10
				if test.oversizedSource {
					bytes = 1 << 20
				}
				source := automatic("review_latest_source", test.sourceVerdict, true, bytes)
				verdict := test.acceptanceVerdict
				if verdict == "" {
					verdict = "pass"
				}
				bytes = 160 << 10
				if test.oversizedAcceptance {
					bytes = 1 << 20
				}
				acceptance := automatic("review_latest_acceptance", verdict, false, bytes)
				if test.sourceAfterValidator {
					history = append(history, acceptance, bot, source)
					want = append(want, acceptance, bot, source)
				} else {
					history = append(history, source, bot, acceptance)
					want = append(want, source, bot, acceptance)
				}
			} else {
				history, want = []tracker.ChangeReview{human, bot}, []tracker.ChangeReview{human, bot}
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			for _, review := range history {
				if err := insertChangeEvidence(t.Context(), tx, f.change.ID, version.ID, "review", review.ID, review); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			before := f.detail(t)
			if !reflect.DeepEqual(before.Reviews, history) {
				t.Fatal("stored review history did not match the retained fixture")
			}
			server := httptest.NewServer(f.service.Handler())
			t.Cleanup(server.Close)
			client, err := hubclient.New(hubclient.Config{URL: server.URL, TokenSource: func() string { return f.token }, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native(f.project.OrganizationID, f.project.ID)
			if err != nil {
				t.Fatal(err)
			}
			recovery, err := native.Recovery(t.Context(), f.issue.WorkItemID)
			if test.oversizedSource || test.oversizedAcceptance || test.oversizedHuman {
				if err == nil || !strings.Contains(err.Error(), "response exceeds 1048576 bytes") {
					t.Fatalf("oversized authoritative evidence must remain an explicit refusal: %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("superseded automatic review output blocked Recovery: %v", err)
				}
				if recovery.ChangeDetail == nil || !reflect.DeepEqual(recovery.ChangeDetail.Reviews, want) || !reflect.DeepEqual(recovery.ChangeDetail.Versions, before.Versions) || !reflect.DeepEqual(recovery.ChangeDetail.Summary, before.Summary) || recovery.Issue.WorkItemID != f.issue.WorkItemID {
					t.Fatal("Recovery changed current review order, ordinary feedback, source or summary authority")
				}
			}
			if after := f.detail(t); !reflect.DeepEqual(after, before) {
				t.Fatal("Recovery altered complete durable Change evidence")
			}
		})
	}
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

func TestChangePublicationPushRetainsScopedCheckpoint(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t, nil)
	descriptor := hubTestPolicy()
	descriptor.Gates.GitHubPullRequest = true
	descriptor = descriptor.WithID()
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: hubTestPolicy().ID, Policy: descriptor}), http.StatusOK)
	rules := f.rules
	rules.PolicyID = descriptor.ID
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "push-policy"}, ExpectedID: f.rules.ID, Policy: rules}), http.StatusOK)
	worker := f.worker(t, "push-worker")
	lease := claimNativeAttempt(t, f.nativeFixture, worker, "push-machine", "push-session", f.issue.WorkItemID, descriptor.ID)
	start := nativeStartedEvent(lease)
	path := f.base + "/work-items/" + string(f.issue.WorkItemID)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, start), http.StatusOK)
	input := changeTestInput()
	input.PolicyID, input.RunID, input.AttemptID = descriptor.ID, start.Data.RunID, start.Data.AttemptID
	request := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "push-source", LeaseID: lease.ID, FencingToken: lease.FencingToken}, ChangeVersionInput: input, SourceBundle: []byte("retained source")}
	request.Source = &tracker.ChangeSource{Format: "git-bundle", BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA, BundleSHA256: tracker.ChangeSourceDigest(request.SourceBundle), DiffSHA256: strings.Repeat("d", 64), Bytes: int64(len(request.SourceBundle))}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", worker, request)
	requireNativeStatus(t, response, http.StatusOK)
	var version tracker.ChangeVersion
	decodeHubResponse(t, response, &version)
	checkpoint := start
	checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Sequence = "run.checkpointed", "ordinary-checkpoint", 2
	checkpoint.Data.Handoff = nativeTestCheckpoint()
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, checkpoint), http.StatusOK)
	ref := tracker.NativeChangeReference{ChangeID: version.ChangeID, VersionID: version.ID, HeadSHA: version.HeadSHA}
	binding := tracker.NativePRPublication{ChangeID: version.ChangeID, VersionID: version.ID, Repository: version.Repository, HeadSHA: version.HeadSHA, PolicyID: version.PolicyID, BaseRef: "main", Branch: "retained", SourceVersion: ref, SourceAttemptID: version.AttemptID}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	foreignProject := newNativeFixture(t, f.service, f.project.OrganizationID, "other-push-project")
	foreignOrganization := newNativeFixture(t, f.service, "", "other-push-organization")
	foreignProjectItem := foreignProject.create(t, "foreign project work")
	foreignOrganizationItem := foreignOrganization.create(t, "foreign organization work")
	foreignItem := f.create(t, "foreign work")
	baseline := f.detail(t)
	var snapshot string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT record_json FROM change_versions WHERE id=?", version.ID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		want bool
	}{
		{"confirmed", true}, {"overwritten latest checkpoint", true}, {"compacted history", true},
		{"pending", false}, {"ambiguous", false}, {"missing checkpoint", false},
		{"foreign organization", false}, {"foreign project", false}, {"foreign work item", false},
		{"wrong recorder", false}, {"wrong run", false}, {"wrong lease", false}, {"wrong fence", false},
		{"wrong policy", false}, {"wrong pinned policy", false}, {"wrong attempt policy", false}, {"unrecorded sequence", false},
		{"wrong change", false}, {"wrong version", false}, {"wrong reference head", false}, {"wrong checkpoint head", false},
		{"latest malformed recorder", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			run := checkpoint.Data
			handoff := *checkpoint.Data.Handoff
			reference := ref
			handoff.Change, handoff.HeadSHA = &reference, version.HeadSHA
			handoff.ExternalEffect, handoff.EffectState, handoff.EffectID = "git_push", "confirmed", binding.EffectID("git_push")
			run.Handoff = &handoff
			eventScope, eventItem := scope, f.issue.WorkItemID
			appendCheckpoint := func() {
				t.Helper()
				if err := appendNativeHistory(t.Context(), tx, eventScope, string(eventItem), "run.checkpointed", tracker.CollaborationData{Run: &run}, f.service.config.now()); err != nil {
					t.Fatal(err)
				}
			}
			// Inject corrupt retained rows below the validated event API to prove
			// the read boundary rejects mismatched attribution independently.
			switch test.name {
			case "pending", "ambiguous":
				handoff.EffectState = test.name
			case "missing checkpoint":
				run.Handoff = nil
			case "foreign organization":
				eventScope = nativeScope{organization: foreignOrganization.project.OrganizationID, project: foreignOrganization.project.ID}
				eventItem = foreignOrganizationItem.WorkItemID
			case "foreign project":
				eventScope.project, eventItem = foreignProject.project.ID, foreignProjectItem.WorkItemID
			case "foreign work item":
				eventItem = foreignItem.WorkItemID
			case "latest malformed recorder":
				appendCheckpoint()
				fallthrough
			case "wrong recorder":
				run.AttemptID = newNativeID("attempt")
			case "wrong run":
				run.RunID = newNativeID("run")
			case "wrong lease":
				run.LeaseID = "foreign-lease"
			case "wrong fence":
				run.FencingToken++
			case "wrong policy":
				run.PolicyID = hubTestPolicy().ID
			case "wrong pinned policy":
				if _, err := tx.ExecContext(t.Context(), "UPDATE lease_policies SET policy_id=? WHERE lease_id=?", hubTestPolicy().ID, lease.ID); err != nil {
					t.Fatal(err)
				}
			case "wrong attempt policy":
				if _, err := tx.ExecContext(t.Context(), "UPDATE native_attempts SET data_json=json_set(data_json,'$.policy_id',?) WHERE id=?", hubTestPolicy().ID, run.AttemptID); err != nil {
					t.Fatal(err)
				}
			case "unrecorded sequence":
				run.Sequence++
			case "wrong change":
				reference.ChangeID = newNativeID("change")
			case "wrong version":
				reference.VersionID = newNativeID("version")
			case "wrong reference head":
				reference.HeadSHA = strings.Repeat("e", 40)
			case "wrong checkpoint head":
				handoff.HeadSHA = strings.Repeat("e", 40)
			}
			appendCheckpoint()
			if test.name == "overwritten latest checkpoint" {
				if _, err := tx.ExecContext(t.Context(), "UPDATE native_attempts SET checkpoint_json=NULL WHERE id=?", run.AttemptID); err != nil {
					t.Fatal(err)
				}
			}
			if test.name == "compacted history" {
				if _, err := tx.ExecContext(t.Context(), "UPDATE issues SET archived=1 WHERE native_id=?", f.issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), "UPDATE leases SET released_at=? WHERE lease_id=?", formatHubTime(f.service.config.now()), lease.ID); err != nil {
					t.Fatal(err)
				}
				if _, _, err := compactEvents(t.Context(), tx, f.service.config.now().Add(31*24*time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			proof, found, err := resolveVersionPublicationPush(t.Context(), tx, scope, string(f.issue.WorkItemID), version)
			if err != nil || found != test.want {
				t.Fatalf("proof=%+v want=%v error=%v", proof, test.want, err)
			}
			if found && (proof.Change != ref || proof.RecordingAttemptID != start.Data.AttemptID || proof.EffectID != binding.EffectID("git_push")) {
				t.Fatalf("checkpoint attribution changed: %+v", proof)
			}
			detail, err := readChangeDetail(t.Context(), tx, scope, string(f.issue.WorkItemID), version.ChangeID, f.service.config.now())
			if err != nil || !reflect.DeepEqual(detail.Versions, baseline.Versions) || !reflect.DeepEqual(detail.Reviews, baseline.Reviews) || !reflect.DeepEqual(detail.Summary, baseline.Summary) || detail.External != nil {
				t.Fatalf("push proof mutated source or review evidence: detail=%+v error=%v", detail, err)
			}
			var after string
			if err := tx.QueryRowContext(t.Context(), "SELECT record_json FROM change_versions WHERE id=?", version.ID).Scan(&after); err != nil || after != snapshot {
				t.Fatalf("immutable source snapshot changed: error=%v", err)
			}
		})
	}
	rows, err := f.service.database.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+versionPublicationPushSQL, scope.organization, scope.project, f.issue.WorkItemID, version.ChangeID, version.ID, version.HeadSHA, scope.organization, scope.project, f.issue.WorkItemID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var description string
		if err := rows.Scan(&id, &parent, &unused, &description); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(description + "\n")
	}
	if err := rows.Err(); err != nil || !strings.Contains(plan.String(), "USING INDEX collaboration_events_publication_push") || strings.Contains(plan.String(), "SCAN collaboration_events") || strings.Contains(plan.String(), "USE TEMP B-TREE") {
		t.Fatalf("checkpoint lookup is not indexed and bounded: %s error=%v", plan.String(), err)
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
		{"unknown configuration", func(v *tracker.ChangeVersionInput) { v.PolicyID = "other" }, http.StatusUnprocessableEntity},
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

type landingCapacityFixture struct {
	nativeFixture
	descriptor policy.Descriptor
}

func newLandingCapacityFixture(t *testing.T, service *Service, name string, github bool) landingCapacityFixture {
	t.Helper()
	f := newNativeFixture(t, service, "", name)
	states := HostedProjectStates()
	raw, err := marshalNative(states)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM workflow_states WHERE project_id=?", f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET states_json=? WHERE id=?", raw, f.project.ID); err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states (project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES (?,?,?,?,?,?,?)", f.project.ID, state.Name, state.Name, state.Terminal, state.Dispatchable, testTimestamp, testTimestamp); err != nil {
			t.Fatal(err)
		}
	}
	descriptor := hubTestPolicy()
	descriptor.Gates.GitHubPullRequest = github
	descriptor = descriptor.WithID()
	approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
	return landingCapacityFixture{nativeFixture: f, descriptor: descriptor}
}

func (f landingCapacityFixture) ready(t *testing.T, name string) tracker.NativeIssue {
	t.Helper()
	response := createIndependentNativeIssue(t, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-" + name}, Title: name, Body: issueContractTestSections, State: "Merging"}, func(request tracker.CreateIssue) *httptest.ResponseRecorder {
		return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
	})
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	path := f.base + "/work-items/" + string(issue.WorkItemID) + "/changes"
	response = performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change-" + name}, Title: name})
	requireNativeStatus(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	decodeHubResponse(t, response, &change)
	input := changeTestInput()
	input.PolicyID = f.descriptor.ID
	bundle := []byte("retained admission fixture")
	input.Source = &tracker.ChangeSource{Format: "git-bundle", BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA, BundleSHA256: tracker.ChangeSourceDigest(bundle), DiffSHA256: strings.Repeat("d", 64), Bytes: int64(len(bundle))}
	path += "/" + change.ID
	response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "publish-" + name}, ChangeVersionInput: input, SourceBundle: bundle})
	requireNativeStatus(t, response, http.StatusOK)
	var version tracker.ChangeVersion
	decodeHubResponse(t, response, &version)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "review-" + name}, Decision: "approved"}), http.StatusOK)
	return issue
}

func (f landingCapacityFixture) claim(t *testing.T, r runnerFixture, issue tracker.NativeIssue, session string, capacity int) *httptest.ResponseRecorder {
	t.Helper()
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, tracker.NativeClaim{LandingCapacity: capacity, PolicyID: f.descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: session, TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}})
}

func TestGitHubLandingCapacityAcrossRunners(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                                                                                                                            string
		ownerCapacity, contenderCapacity                                                                                                int
		boundary                                                                                                                        string
		nativeOnly, sameBatch, otherProject, laneChanged, expired, concurrent, fallback, room, renewFree, renewRace, direct, directRace bool
	}{
		{name: "one project across hosts", ownerCapacity: 1, contenderCapacity: 1},
		{name: "legacy owner occupies a slot", contenderCapacity: 1},
		{name: "legacy contender cannot bypass owner ceiling", ownerCapacity: 1},
		{name: "wider contender preserves owner ceiling", ownerCapacity: 1, contenderCapacity: 3},
		{name: "lower contender retains its ceiling", ownerCapacity: 3, contenderCapacity: 1},
		{name: "configured multiple slots permit capacity", ownerCapacity: 2, contenderCapacity: 2, room: true},
		{name: "legacy unconfigured claims are unchanged", room: true},
		{name: "lane change retains live occupancy", ownerCapacity: 1, contenderCapacity: 1, laneChanged: true},
		{name: "expired owner frees occupancy", ownerCapacity: 1, contenderCapacity: 1, expired: true},
		{name: "expired owner may renew a free slot", ownerCapacity: 1, contenderCapacity: 1, expired: true, renewFree: true},
		{name: "expired changed-lane owner cannot renew an occupied slot", ownerCapacity: 1, contenderCapacity: 1, expired: true, laneChanged: true},
		{name: "negative capacity is invalid", ownerCapacity: -1},
		{name: "separate project proceeds", ownerCapacity: 1, contenderCapacity: 1, otherProject: true},
		{name: "native-only landing is unchanged", ownerCapacity: 1, contenderCapacity: 1, nativeOnly: true},
		{name: "native batch shares its host slot", ownerCapacity: 1, contenderCapacity: 1, nativeOnly: true, sameBatch: true},
		{name: "concurrent claims acquire one slot", ownerCapacity: 1, contenderCapacity: 1, concurrent: true},
		{name: "expired renewal races a new claim", ownerCapacity: 1, contenderCapacity: 1, concurrent: true, renewRace: true},
		{name: "busy landing permits other ready work", ownerCapacity: 1, contenderCapacity: 1, fallback: true},
		{name: "direct landing shares project admission", ownerCapacity: 1, contenderCapacity: 1, direct: true},
		{name: "concurrent direct transitions acquire one slot", ownerCapacity: 1, contenderCapacity: 1, concurrent: true, directRace: true},
		{name: "integral current second retains fractional future expiry", ownerCapacity: 1, contenderCapacity: 1, boundary: "integral"},
		{name: "fractional current second retains nanosecond future expiry", ownerCapacity: 1, contenderCapacity: 1, boundary: "fractional"},
		{name: "exact expiry frees capacity", ownerCapacity: 1, contenderCapacity: 1, boundary: "exact", expired: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var service *Service
			if test.boundary != "" {
				now := time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)
				if test.boundary == "fractional" {
					now = now.Add(800 * time.Millisecond)
				}
				service = openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }})
			}
			f := newLandingCapacityFixture(t, service, "landing-capacity", !test.nativeOnly)
			first := f.ready(t, "first change")
			secondFixture := f
			if test.otherProject {
				secondFixture = newLandingCapacityFixture(t, f.service, "independent-landings", true)
			}
			second := secondFixture.ready(t, "second change")
			if test.direct {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='In Progress') WHERE native_id=?", f.project.ID, second.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			if test.directRace {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='In Progress') WHERE native_id IN (?,?)", f.project.ID, first.WorkItemID, second.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			owner := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim)
			owner.enroll(t)
			waiter := prepareRunner(t, secondFixture.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Events)
			waiter.enroll(t)
			ownerSession, waiterSession := "first-landing", "waiting-landing"
			if test.ownerCapacity < 0 {
				requireNativeStatus(t, f.claim(t, owner, first, ownerSession, test.ownerCapacity), http.StatusUnprocessableEntity)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims/preview", owner.redemption.Credential, tracker.NativeCapacityPreview{NativeClaim: tracker.NativeClaim{LandingCapacity: test.ownerCapacity, PolicyID: f.descriptor.ID, MachineID: owner.binding.MachineID}}), http.StatusUnprocessableEntity)
				return
			}
			if test.sameBatch {
				waiter = owner
				ownerSession, waiterSession = "landing/shared/owner", "landing/shared/waiter"
			}
			if test.concurrent {
				acquire := func() *httptest.ResponseRecorder { return f.claim(t, owner, first, ownerSession, test.ownerCapacity) }
				contend := func() *httptest.ResponseRecorder {
					return secondFixture.claim(t, waiter, second, waiterSession, test.contenderCapacity)
				}
				if test.renewRace {
					response := acquire()
					requireNativeStatus(t, response, http.StatusOK)
					var lease tracker.NativeLease
					decodeHubResponse(t, response, &lease)
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE leases SET expires_at=? WHERE lease_id=?", formatHubTime(f.service.config.now().Add(-time.Second)), lease.ID); err != nil {
						t.Fatal(err)
					}
					acquire = func() *httptest.ResponseRecorder {
						return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", owner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90})
					}
				}
				if test.directRace {
					firstResponse, secondResponse := acquire(), contend()
					requireNativeStatus(t, firstResponse, http.StatusOK)
					requireNativeStatus(t, secondResponse, http.StatusOK)
					var firstLease, secondLease tracker.NativeLease
					decodeHubResponse(t, firstResponse, &firstLease)
					decodeHubResponse(t, secondResponse, &secondLease)
					acquire = func() *httptest.ResponseRecorder {
						return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(firstLease.ID)+"/renew", owner.redemption.Credential, tracker.NativeLeaseMutation{LandingCapacity: test.ownerCapacity, FencingToken: firstLease.FencingToken, TTLSeconds: 90})
					}
					contend = func() *httptest.ResponseRecorder {
						return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(secondLease.ID)+"/renew", waiter.redemption.Credential, tracker.NativeLeaseMutation{LandingCapacity: test.contenderCapacity, FencingToken: secondLease.FencingToken, TTLSeconds: 90})
					}
				}
				start := make(chan struct{})
				responses := make(chan *httptest.ResponseRecorder, 2)
				var workers sync.WaitGroup
				workers.Go(func() { <-start; responses <- acquire() })
				workers.Go(func() {
					<-start
					responses <- contend()
				})
				close(start)
				workers.Wait()
				close(responses)
				winners := 0
				for response := range responses {
					if response.Code == http.StatusOK {
						winners++
					} else {
						requireNativeStatus(t, response, http.StatusConflict)
					}
				}
				if winners != 1 {
					t.Fatalf("concurrent landing owners = %d, want 1", winners)
				}
				return
			}
			response := f.claim(t, owner, first, ownerSession, test.ownerCapacity)
			requireNativeStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			replay := f.claim(t, owner, first, ownerSession, test.ownerCapacity+1)
			requireNativeStatus(t, replay, http.StatusOK)
			var replayed tracker.NativeLease
			decodeHubResponse(t, replay, &replayed)
			if replayed.ID != lease.ID || replayed.FencingToken != lease.FencingToken {
				t.Fatal("claim replay changed the lease owner or fence")
			}
			var pinned int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT landing_capacity FROM leases WHERE lease_id=?", lease.ID).Scan(&pinned); err != nil {
				t.Fatal(err)
			}
			wantPinned := test.ownerCapacity
			if test.nativeOnly {
				wantPinned = 0
			}
			if pinned != wantPinned {
				t.Fatalf("claim replay widened the owner ceiling to %d, want %d", pinned, wantPinned)
			}
			if test.direct {
				response := f.claim(t, waiter, second, waiterSession, test.contenderCapacity)
				requireNativeStatus(t, response, http.StatusOK)
				var direct tracker.NativeLease
				decodeHubResponse(t, response, &direct)
				admit := func(capacity int) *httptest.ResponseRecorder {
					return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(direct.ID)+"/renew", waiter.redemption.Credential, tracker.NativeLeaseMutation{LandingCapacity: capacity, FencingToken: direct.FencingToken, TTLSeconds: 90})
				}
				requireNativeStatus(t, admit(-1), http.StatusUnprocessableEntity)
				requireNativeStatus(t, admit(test.contenderCapacity), http.StatusConflict)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(direct.ID)+"/validate", waiter.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: direct.FencingToken}), http.StatusOK)
				var change, version string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT id,json_extract(record_json,'$.current_version_id') FROM change_requests WHERE work_item_id=?", second.WorkItemID).Scan(&change, &version); err != nil {
					t.Fatal(err)
				}
				event := nativeStartedEvent(direct)
				path := f.base + "/work-items/" + string(second.WorkItemID)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", waiter.redemption.Credential, event), http.StatusOK)
				event.Type, event.IdempotencyKey, event.Data.Sequence = "run.observed", "landing-wait", 2
				event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: f.service.config.now(), Landing: &tracker.NativeLandingReceipt{ChangeID: change, VersionID: version, HeadSHA: changeTestInput().HeadSHA, Waiting: true}}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", waiter.redemption.Credential, event), http.StatusOK)
				event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome = "run.finished", "finish-wait", 3, "succeeded"
				event.Data.Disposition = &tracker.NativeDisposition{Status: "in_progress"}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", waiter.redemption.Credential, event), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(direct.ID)+"/release", waiter.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: direct.FencingToken, Reason: "completed"}), http.StatusNoContent)
				requireNativeStatus(t, f.claim(t, waiter, second, "resumed-direct-landing", test.contenderCapacity), http.StatusConflict)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", owner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
				response = f.claim(t, waiter, second, "resumed-direct-landing", test.contenderCapacity)
				requireNativeStatus(t, response, http.StatusOK)
				prior := direct
				decodeHubResponse(t, response, &direct)
				if direct.ID == prior.ID || direct.FencingToken <= prior.FencingToken {
					t.Fatal("waiting direct landing did not acquire a new fenced owner")
				}
				response = performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var projected tracker.NativeIssue
				decodeHubResponse(t, response, &projected)
				if projected.ListChange == nil || !projected.ListChange.LandingPending || projected.ListChange.HeadSHA != changeTestInput().HeadSHA {
					t.Fatalf("waiting direct landing lost source intent: %#v", projected.ListChange)
				}
				requireNativeStatus(t, admit(test.contenderCapacity), http.StatusOK)
				requireNativeStatus(t, admit(test.contenderCapacity+1), http.StatusOK)
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT landing_capacity FROM leases WHERE lease_id=?", direct.ID).Scan(&pinned); err != nil {
					t.Fatal(err)
				}
				if pinned != test.contenderCapacity {
					t.Fatalf("direct admission replay changed ceiling to%d, want%d", pinned, test.contenderCapacity)
				}
				third := f.ready(t, "third change")
				requireNativeStatus(t, f.claim(t, owner, third, "next-landing", test.ownerCapacity), http.StatusConflict)
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET revision=revision+1 WHERE native_id=?", second.WorkItemID); err != nil {
					t.Fatal(err)
				}
				response = performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				projected = tracker.NativeIssue{}
				decodeHubResponse(t, response, &projected)
				if projected.ListChange != nil && projected.ListChange.LandingPending {
					t.Fatal("operator revision change retained stale direct-landing intent")
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(direct.ID)+"/release", waiter.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: direct.FencingToken, Reason: "completed"}), http.StatusNoContent)
				requireNativeStatus(t, f.claim(t, owner, third, "next-landing", test.ownerCapacity), http.StatusOK)
				return
			}
			if test.laneChanged {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='In Progress') WHERE native_id=?", f.project.ID, first.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			if test.expired {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE leases SET expires_at=? WHERE lease_id=?", formatHubTime(f.service.config.now().Add(-time.Second)), lease.ID); err != nil {
					t.Fatal(err)
				}
			}
			if test.boundary != "" {
				expiry := f.service.config.now()
				switch test.boundary {
				case "integral":
					expiry = expiry.Add(100 * time.Millisecond)
				case "fractional":
					expiry = expiry.Add(time.Nanosecond)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE leases SET expires_at=? WHERE lease_id=?", formatHubTime(expiry), lease.ID); err != nil {
					t.Fatal(err)
				}
			}
			if test.renewFree {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", owner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), http.StatusOK)
			}
			want := http.StatusConflict
			if test.nativeOnly || test.otherProject || test.expired && !test.renewFree || test.room {
				want = http.StatusOK
			}
			preview := performHubAPIRequest(t, f.service, http.MethodPost, secondFixture.base+"/claims/preview", waiter.redemption.Credential, tracker.NativeCapacityPreview{NativeClaim: tracker.NativeClaim{LandingCapacity: test.contenderCapacity, PolicyID: secondFixture.descriptor.ID, MachineID: waiter.binding.MachineID}})
			requireNativeStatus(t, preview, http.StatusOK)
			var page tracker.NativeCapacityPage
			decodeHubResponse(t, preview, &page)
			visible := false
			for _, item := range page.Items {
				visible = visible || item.WorkItemID == second.WorkItemID
			}
			if visible != (want == http.StatusOK) {
				t.Fatalf("preview landing eligibility = %v, claim status = %d", visible, want)
			}
			requireNativeStatus(t, secondFixture.claim(t, waiter, second, waiterSession, test.contenderCapacity), want)
			if want == http.StatusConflict && test.ownerCapacity == 1 {
				response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(second.WorkItemID)+"/runtime", f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var evidence tracker.NativeRuntimeEvidence
				decodeHubResponse(t, response, &evidence)
				if len(evidence.Admission) == 0 {
					t.Fatal("runtime read lost enrolled-runner capacity evidence")
				}
				for _, admission := range evidence.Admission {
					if admission.Outcome != "skipped" || admission.Reason != "Project landing capacity is full" {
						t.Fatalf("runtime admission disagrees with occupied project capacity: %#v", admission)
					}
				}
			}
			if test.sameBatch {
				tx, err := f.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				available, err := machineClaimCapacity(t.Context(), tx, owner.binding.MachineID, f.service.config.now())
				if rollbackErr := tx.Rollback(); err != nil || rollbackErr != nil {
					t.Fatal(err, rollbackErr)
				}
				if available != 1 {
					t.Fatalf("two native batch owners consumed more than one host slot: available=%d, want1", available)
				}
			}
			if test.fallback {
				todo := f.createIndependent(t, "source work")
				response = f.claim(t, waiter, tracker.NativeIssue{}, "other-ready-work", test.contenderCapacity)
				requireNativeStatus(t, response, http.StatusOK)
				var other tracker.NativeLease
				decodeHubResponse(t, response, &other)
				if other.WorkItemID != todo.WorkItemID {
					t.Fatalf("busy landing stranded ready source work: %#v", other)
				}
			}
			if test.expired && !test.renewFree {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", owner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), http.StatusConflict)
			} else {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", owner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", owner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
			}
			response = secondFixture.claim(t, waiter, second, waiterSession, test.contenderCapacity)
			requireNativeStatus(t, response, http.StatusOK)
			var resumed tracker.NativeLease
			decodeHubResponse(t, response, &resumed)
			if resumed.WorkItemID != second.WorkItemID || resumed.MachineID != waiter.binding.MachineID || resumed.ID == lease.ID {
				t.Fatalf("next landing did not acquire its own fenced lease: %#v", resumed)
			}
			if test.room && test.ownerCapacity > 0 {
				third := f.ready(t, "third change")
				requireNativeStatus(t, f.claim(t, owner, third, "third-landing", test.ownerCapacity), http.StatusOK)
				fourth := f.ready(t, "fourth change")
				requireNativeStatus(t, f.claim(t, owner, fourth, "fourth-landing", test.ownerCapacity), http.StatusConflict)
			}
		})
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
		name     string
		states   []tracker.NativeState
		from     string
		want     string
		provider bool
	}{
		{name: "hosted template lands", states: HostedProjectStates(), from: "Human Review", want: "Merging"},
		{name: "provider candidate lands", states: HostedProjectStates(), from: "Human Review", want: "Merging", provider: true},
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
				if test.provider {
					publishCapacity(t, f, r, capacityReport(f.service.config.now()))
					claim.WorkItemID = ""
					claim.SessionID = "landing/batch/candidate"
					claim.ProviderCandidates = []tracker.NativeCapacityCandidate{{WorkItemID: issue.WorkItemID, Revision: issue.Revision, Requirement: providercapacity.Requirement{Role: "merge", Backend: "codex", Model: "test-model"}}}
				}
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
				if test.provider {
					claim.ProviderCandidates[0].Revision = issue.Revision + 1
				}
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
		from        string
		want        string
		landingMode string
	}{
		{from: "Human Review", want: "Merging"},
		{from: "In Progress", want: "In Progress"},
		{from: "In Progress", want: "In Progress", landingMode: "rolling_barrier"},
	} {
		t.Run(test.from+"/"+test.landingMode, func(t *testing.T) {
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
			descriptor := hubTestPolicy()
			if test.landingMode != "" {
				descriptor.Gates.LandingMode = test.landingMode
				descriptor.Gates.LandingCommandDigest = policy.Digest([]byte("make check-barrier"))
				descriptor = descriptor.WithID()
			}
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "work"}, Title: "Land me", State: test.from})
			requireNativeStatus(t, response, http.StatusOK)
			var issue tracker.NativeIssue
			decodeHubResponse(t, response, &issue)
			var worker string
			var lease tracker.NativeLease
			if test.landingMode == "rolling_barrier" {
				worker = f.worker(t, "legacy-worker")
				lease = claimNativeAttempt(t, f, worker, "legacy-machine", "legacy-session", issue.WorkItemID, descriptor.ID)
			}
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/changes"
			response = performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Native change"})
			requireNativeStatus(t, response, http.StatusOK)
			var change tracker.ChangeRequest
			decodeHubResponse(t, response, &change)
			path += "/" + change.ID
			input := changeTestInput()
			input.PolicyID = descriptor.ID
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "publish"}, ChangeVersionInput: input}), http.StatusOK)
			response = performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var detail tracker.ChangeDetail
			decodeHubResponse(t, response, &detail)
			wantStatus := "reviewed"
			if test.landingMode == "rolling_barrier" {
				wantStatus = "stale_policy"
				request := tracker.LandChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "legacy-landing", LeaseID: lease.ID, FencingToken: lease.FencingToken}, MergeSHA: strings.Repeat("e", 40), BaseRef: "main", Method: "squash"}
				refused := performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+detail.Change.CurrentVersion+"/landing", worker, request)
				requireNativeStatus(t, refused, http.StatusConflict)
				if !strings.Contains(refused.Body.String(), `"code":"not_reviewed"`) {
					t.Fatalf("legacy landing did not reach review authority: %s", refused.Body.String())
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), http.StatusOK)
			}
			if detail.Summary.Status != wantStatus || detail.Summary.NativeReview != "not_required" {
				t.Fatalf("summary under the default policy = %#v, want %s with no review required", detail.Summary, wantStatus)
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

func TestRetiredLandingBarrierEndpoints(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, method, f.base+"/landing-barrier", f.token, nil), http.StatusNotFound)
		})
	}
}
