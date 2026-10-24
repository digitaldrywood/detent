package hubserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
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
		bundle = changeTestSource(t, &input)
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
	current, err := readCurrentChangeDetail(f.service.contentContext(t.Context()), f.service.database.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, detail.Change, f.service.config.now())
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
		measuredShape        bool
		historyPressure      bool
		historicalReview     bool
		aggregatePressure    bool
		fragmentBody         bool
	}{
		{name: "fresh acceptance follows source", sourceVerdict: "pass"},
		{name: "source pass requires newer acceptance", sourceVerdict: "pass", sourceAfterValidator: true},
		{name: "latest source wait is retained", sourceVerdict: "wait", sourceAfterValidator: true},
		{name: "latest source rework is retained", sourceVerdict: "rework", sourceAfterValidator: true},
		{name: "latest acceptance wait is retained", sourceVerdict: "pass", acceptanceVerdict: "wait"},
		{name: "latest acceptance rework is retained", sourceVerdict: "pass", acceptanceVerdict: "rework"},
		{name: "ordinary reviews remain complete", ordinaryOnly: true},
		{name: "combined legal evidence fits the aggregate response", ordinaryOnly: true, aggregatePressure: true},
		{name: "near limit instructions are restored before dispatch", ordinaryOnly: true, aggregatePressure: true, fragmentBody: true},
		{name: "measured retained review shape", sourceVerdict: "pass", measuredShape: true},
		{name: "retained human hold outside context budget", sourceVerdict: "pass", historyPressure: true},
		{name: "historical review keeps its own version", sourceVerdict: "pass", historicalReview: true},
		{name: "single authoritative source remains oversized", sourceVerdict: "pass", oversizedSource: true},
		{name: "single authoritative acceptance remains oversized", sourceVerdict: "pass", oversizedAcceptance: true},
		{name: "ordinary human review remains oversized", sourceVerdict: "pass", oversizedHuman: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newChangeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")}))
			var version, historical tracker.ChangeVersion
			if test.historicalReview {
				historical = f.publish(t, "historical-version", "", true)
			}
			if test.measuredShape || test.aggregatePressure {
				cfg := config.Default()
				cfg.Tracker.Kind = config.TrackerHubNative
				promptBytes := 48_469
				if test.aggregatePressure {
					promptBytes = 320 << 10
				}
				descriptor, err := config.ResolvePolicy(config.Workflow{Config: cfg, Prompt: strings.Repeat("p", promptBytes)})
				if err != nil {
					t.Fatal(err)
				}
				approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
				if test.aggregatePressure {
					rules := f.rules
					rules.PolicyID = descriptor.ID
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "aggregate-rules"}, ExpectedID: f.rules.ID, Policy: rules}), http.StatusOK)
					body := f.issue.Body + strings.Repeat("<", (64<<10)-len(f.issue.Body))
					if test.fragmentBody {
						body = f.issue.Body + strings.Repeat("<", 153<<10) + strings.Repeat("b", (256<<10)-len(f.issue.Body)-(153<<10))
					}
					update := tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "aggregate-body"}, ExpectedRevision: f.issue.Revision, Body: &body}
					encoded, err := json.Marshal(update)
					if err != nil || len(encoded) >= maxAPIRequestBodyBytes {
						t.Fatalf("illegal issue request: %d, %v", len(encoded), err)
					}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(f.issue.WorkItemID), f.token, update), http.StatusOK)
				}
				input := changeTestInput()
				input.PolicyID = descriptor.ID
				bundle := changeTestSource(t, &input)
				input.Validation = &gate.CommandResult{Command: "make check-land", HeadSHA: input.HeadSHA, TreeSHA: strings.Repeat("d", 40), DurationNS: 1, Output: strings.Repeat("v", 64_000) + strings.Repeat("<", 450)}
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "retained-version"}, ChangeVersionInput: input, SourceBundle: bundle})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &version)
				worker := f.worker(t, "retained-worker")
				lease := claimNativeAttempt(t, f.nativeFixture, worker, "retained-machine", "retained-session", f.issue.WorkItemID, descriptor.ID)
				start := nativeStartedEvent(lease)
				itemPath := f.base + "/work-items/" + string(f.issue.WorkItemID)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", worker, start), http.StatusOK)
				finish := start
				finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", "finish", 2, "succeeded"
				finish.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", Validation: &gate.CommandResult{Command: "make check-land", HeadSHA: version.HeadSHA, TreeSHA: strings.Repeat("d", 40), DurationNS: 1, Output: strings.Repeat("v", 64_000) + strings.Repeat("<", 1000)}}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", worker, finish), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
				if test.measuredShape {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "discussion"}, Body: strings.Repeat("d", 32_080)}), http.StatusOK)
				}
			} else {
				version = f.publish(t, "retained-version", historical.ID, true)
			}
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
				if test.measuredShape {
					bytes = 579_328
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
				if test.measuredShape {
					bytes = 285_066
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
			if test.historicalReview {
				review := bot
				review.ID, review.VersionID, review.Decision = "review_historical", historical.ID, "changes_requested"
				history = append(history, review)
			}
			if test.historyPressure {
				for i := range 96 {
					review := bot
					review.ID = fmt.Sprintf("review_context_%d", i)
					history = append(history, review)
					want = append(want, review)
				}
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			for _, review := range history {
				if err := insertChangeEvidence(t.Context(), tx, f.change.ID, review.VersionID, "review", review.ID, review); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if test.aggregatePressure {
				for i := range 3 {
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions/"+version.ID+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("aggregate-review-%d", i)}, Decision: "commented", Body: strings.Repeat("r", 60<<10)})
					requireNativeStatus(t, response, http.StatusOK)
					var review tracker.ChangeReview
					decodeHubResponse(t, response, &review)
					history = append(history, review)
					want = append(want, review)
				}
				for i := range 2 {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("aggregate-comment-%d", i)}, Body: strings.Repeat("c", 60<<10)}), http.StatusOK)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/discussion", f.token, tracker.DiscussChange{Mutation: tracker.Mutation{IdempotencyKey: "aggregate-discussion"}, VersionID: version.ID, Body: strings.Repeat("d", 50<<10)}), http.StatusOK)
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
			raw := performHubAPIRequest(t, f.service, http.MethodGet, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/projects/"+string(f.project.ID)+"/work-items/"+string(f.issue.WorkItemID)+"?view=recovery", f.token, nil)
			t.Logf("recovery route bytes=%d reviews=%d", raw.Body.Len(), len(want))
			var exact tracker.NativeRecovery
			if test.measuredShape && raw.Body.Len() <= 1<<20 {
				t.Fatal("measured retained evidence did not reproduce the response refusal")
			}
			decodeHubResponse(t, raw, &exact)
			var bodyCursor string
			if test.aggregatePressure {
				path := f.base + "/work-items/" + string(f.issue.WorkItemID) + "?view=recovery"
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.Header.Set("Authorization", "Bearer "+f.token)
				req.Header.Set("X-Detent-Recovery-Projection", "bounded")
				req.Header.Set("X-Detent-Recovery-Body", "fragments")
				wire := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(wire, req)
				requireNativeStatus(t, wire, http.StatusOK)
				if wire.Body.Len() > 1<<20 {
					t.Fatalf("aggregate response exceeds SDK limit: %d", wire.Body.Len())
				}
				var projected tracker.NativeRecovery
				decodeHubResponse(t, wire, &projected)
				if test.fragmentBody && (projected.Read == nil || projected.Read.IssueBody == nil || projected.Issue.Body != "") {
					t.Fatal("near-limit body did not use required record continuation")
				}
				if projected.Read != nil && projected.Read.IssueBody != nil {
					bodyCursor = projected.Read.IssueBody.Cursor
				}
				legacy := performHubAPIRequest(t, f.service, http.MethodGet, path+"&projection=bounded", f.token, nil)
				if legacy.Code == http.StatusOK {
					var oldClient tracker.NativeRecovery
					decodeHubResponse(t, legacy, &oldClient)
					if oldClient.Issue.Body != exact.Issue.Body || legacy.Body.Len() > 1<<20 || oldClient.Read != nil && oldClient.Read.IssueBody != nil {
						t.Fatal("older SDK received incomplete or oversized successful recovery")
					}
				} else {
					requireNativeStatus(t, legacy, http.StatusUnprocessableEntity)
				}
				t.Logf("legacy=%d bounded=%d bodyFragments=%t", raw.Body.Len(), wire.Body.Len(), projected.Read != nil && projected.Read.IssueBody != nil)
			}
			recovery, err := native.Recovery(t.Context(), f.issue.WorkItemID)
			if err != nil {
				t.Fatalf("retained review evidence blocked Recovery: %v", err)
			}
			if recovery.ChangeDetail == nil || !reflect.DeepEqual(recovery.ChangeDetail.Summary, before.Summary) || !reflect.DeepEqual(recovery.Issue, exact.Issue) || !reflect.DeepEqual(recovery.Change, exact.Change) || recovery.SourceAttemptID != exact.SourceAttemptID {
				t.Fatal("Recovery changed source or summary authority")
			}
			for _, version := range recovery.ChangeDetail.Versions {
				var want tracker.ChangeVersion
				for _, original := range before.Versions {
					if original.ID == version.ID {
						want = original
					}
				}
				if recovery.Read != nil && slices.Contains(recovery.Read.Omitted, "change_detail.versions.policy") {
					if version.Policy.ID != want.Policy.ID || !reflect.DeepEqual(version.Policy.Gates, want.Policy.Gates) || version.Policy.SourceRevision != want.Policy.SourceRevision {
						t.Fatal("projected policy lost identity or gates")
					}
					version.Policy, want.Policy = policy.Descriptor{}, policy.Descriptor{}
				}
				version.Validation, want.Validation = nil, nil
				if !reflect.DeepEqual(version, want) {
					t.Fatal("Recovery changed immutable source or frozen policy")
				}
			}
			for _, review := range recovery.ChangeDetail.Reviews {
				var original tracker.ChangeReview
				for _, candidate := range want {
					if candidate.ID == review.ID {
						original = candidate
					}
				}
				for _, pair := range [][2]*gate.ValidatorResult{{review.ImplementationReview, original.ImplementationReview}, {review.Validator, original.Validator}} {
					if pair[0] == nil || pair[1] == nil {
						continue
					}
					got, expected := *pair[0], *pair[1]
					got.Commands, expected.Commands = nil, nil
					if !reflect.DeepEqual(got, expected) || !pair[0].Commands[0].OutputTruncated || len(pair[0].Commands[0].Output) > 4096 {
						t.Fatal("bounded review lost verdict, version, criteria or output provenance")
					}
				}
				review.ImplementationReview, review.Validator = nil, nil
				original.ImplementationReview, original.Validator = nil, nil
				if !reflect.DeepEqual(review, original) {
					t.Fatal("bounded review changed feedback identity, body or decision")
				}
			}
			if !test.ordinaryOnly {
				ids := map[string]bool{}
				for _, review := range recovery.ChangeDetail.Reviews {
					ids[review.ID] = true
				}
				if !ids["review_latest_source"] || !ids["review_latest_acceptance"] {
					t.Fatal("bounded context lost authoritative review criteria")
				}
			}
			seen := map[string]bool{}
			var record []byte
			cursor := ""
			for pages := 0; ; pages++ {
				if pages > 10*len(history)+32 {
					t.Fatal("record continuation did not terminate")
				}
				page, err := native.RecoveryEvidence(t.Context(), f.issue.WorkItemID, cursor)
				if err != nil {
					t.Fatal(err)
				}
				if page.Section == "complete" {
					break
				}
				if page.Offset != int64(len(record)) || len(page.Data) > 256<<10 {
					t.Fatal("record continuation lost progress or response bound")
				}
				record = append(record, page.Data...)
				if int64(len(record)) == page.TotalBytes {
					digest := sha256.Sum256(record)
					if hex.EncodeToString(digest[:]) != page.SHA256 {
						t.Fatal("record digest mismatch")
					}
					if page.Section == "reviews" {
						var review tracker.ChangeReview
						if err := json.Unmarshal(record, &review); err != nil {
							t.Fatal(err)
						}
						for _, original := range history {
							if original.ID == review.ID && page.VersionID == original.VersionID && reflect.DeepEqual(original, review) {
								seen[review.ID] = true
							}
						}
					}
					if page.Section == "versions" {
						var version tracker.ChangeVersion
						if err := json.Unmarshal(record, &version); err != nil {
							t.Fatal(err)
						}
						var original tracker.ChangeVersion
						for _, candidate := range before.Versions {
							if candidate.ID == version.ID {
								original = candidate
							}
						}
						if !reflect.DeepEqual(version, original) {
							t.Fatal("retained source, policy or validation was not retrievable")
						}
					}
					record = nil
				}
				cursor = page.NextCursor
			}
			if len(seen) != len(history) {
				t.Fatal("retained reviews were not completely retrievable")
			}
			if after := f.detail(t); !reflect.DeepEqual(after, before) {
				t.Fatal("Recovery altered complete durable Change evidence")
			}
			if test.fragmentBody {
				path := f.base + "/work-items/" + string(f.issue.WorkItemID) + "?view=recovery&limit=1&cursor=" + url.QueryEscape(bodyCursor)
				first := performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
				requireNativeStatus(t, first, http.StatusOK)
				var fragment tracker.NativeRecovery
				decodeHubResponse(t, first, &fragment)
				if fragment.Continuation == nil || fragment.Continuation.Revision != exact.Issue.Revision || fragment.Continuation.Section != "issue_body" {
					t.Fatal("required body continuation lost revision binding")
				}
				other := f.create(t, "separate recovery item")
				foreign := f.base + "/work-items/" + string(other.WorkItemID) + "?view=recovery&limit=1&cursor=" + url.QueryEscape(bodyCursor)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, foreign, f.token, nil), http.StatusUnprocessableEntity)
				updated := exact.Issue.Body + "changed"
				updated = updated[len("changed"):]
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(f.issue.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "edit-required-body"}, ExpectedRevision: exact.Issue.Revision, Body: &updated}), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil), http.StatusConflict)
				next := f.base + "/work-items/" + string(f.issue.WorkItemID) + "?view=recovery&limit=1&cursor=" + url.QueryEscape(fragment.Continuation.NextCursor)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, next, f.token, nil), http.StatusConflict)
			}
			if test.measuredShape {
				path := f.base + "/work-items/" + string(f.issue.WorkItemID) + "?view=recovery&limit=1"
				response := performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
				if response.Body.Len() > 1<<20 {
					t.Fatal("continued response exceeded client memory bound")
				}
				var page tracker.NativeRecovery
				decodeHubResponse(t, response, &page)
				if page.Continuation == nil || page.Continuation.NextCursor == "" || len(page.Continuation.Data) > 256<<10 {
					t.Fatal("oversized recovery did not provide a bounded continuation")
				}
				cursor := url.QueryEscape(page.Continuation.NextCursor)
				other := f.create(t, "second item")
				otherPath := f.base + "/work-items/" + string(other.WorkItemID) + "?view=recovery&limit=1&cursor=" + cursor
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, otherPath, f.token, nil), http.StatusUnprocessableEntity)
				otherToken := f.worker(t, "other-reader")
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"&cursor="+cursor, otherToken, nil), http.StatusUnprocessableEntity)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "new-discussion"}, Body: "New human context"}), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"&cursor="+cursor, f.token, nil), http.StatusOK)
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
		{"stale fence before invalid source", func(r *tracker.PublishChangeVersion) { r.FencingToken++; r.SourceBundle = []byte("invalid pack") }, http.StatusConflict},
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
			request.SourceBundle = changeTestSource(t, &request.ChangeVersionInput)
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
	bundle := changeTestSource(t, &input)
	request := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "push-source", LeaseID: lease.ID, FencingToken: lease.FencingToken}, ChangeVersionInput: input, SourceBundle: bundle}
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
				plans, err := planEventCompaction(t.Context(), tx, f.service.config.now().Add(31*24*time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := applyEventCompaction(t.Context(), tx, plans, f.service.config.now().Add(31*24*time.Hour)); err != nil {
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
	t.Run("confirmed PR receipt", func(t *testing.T) {
		binding.External = tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: version.Repository + "/pull/7"}
		checkpoint.Data.Sequence = 3
		checkpoint.IdempotencyKey = "confirmed-pr-effect"
		checkpoint.Data.Handoff.HeadSHA = version.HeadSHA
		checkpoint.Data.Handoff.Change = &binding.SourceVersion
		checkpoint.Data.Handoff.ExternalEffect, checkpoint.Data.Handoff.EffectState, checkpoint.Data.Handoff.EffectID = "pr_create", "confirmed", binding.EffectID("pr_create")
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, checkpoint), http.StatusOK)
		ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
		readReferences := func() operatortool.Result {
			t.Helper()
			raw, err := json.Marshal(operatortool.WorkReadRequest{ProjectID: string(f.project.ID), Reference: string(f.issue.WorkItemID), Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			result, err := (hostedOperatorExecutor{service: f.service}).Execute(ctx, operatortool.Call{Name: operatortool.WorkReferences, Arguments: raw})
			if err != nil {
				t.Fatal(err)
			}
			return result
		}
		if result := readReferences(); strings.Contains(string(result.Content), binding.External.URL) || f.detail(t).Versions[0].External != nil {
			t.Fatal("confirmed effect hash alone acquired PR linkage")
		}
		diff := tracker.AttemptDiffRequest{Producer: tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: start.Data.AttemptID, LeaseID: lease.ID, FencingToken: lease.FencingToken}, Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 4}, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/attempts/"+start.Data.AttemptID+"/diff", worker, diff), http.StatusAccepted)
		for _, name := range []string{"pending", "ambiguous", "failed", "stale version", "stale head", "foreign change", "foreign item", "foreign project", "foreign organization", "repository", "url", "number", "base", "branch", "source attempt", "source version", "policy", "lease", "fence", "attempt", "run"} {
			t.Run(name, func(t *testing.T) {
				event := checkpoint
				event.IdempotencyKey, event.Data.Sequence = "invalid-pr-"+strings.ReplaceAll(name, " ", "-"), 4
				publication := binding
				handoff := *checkpoint.Data.Handoff
				event.Data.Handoff = &handoff
				handoff.Publication = &publication
				target := path + "/events"
				switch name {
				case "pending", "ambiguous":
					handoff.EffectState = name
				case "failed":
					handoff.EffectState = "none"
				case "stale version":
					publication.VersionID, publication.SourceVersion.VersionID = "version_stale", "version_stale"
				case "stale head":
					publication.HeadSHA, publication.SourceVersion.HeadSHA = strings.Repeat("e", 40), strings.Repeat("e", 40)
				case "foreign change":
					publication.ChangeID, publication.SourceVersion.ChangeID = "change_foreign", "change_foreign"
				case "foreign item":
					target = f.base + "/work-items/" + string(foreignItem.WorkItemID) + "/events"
				case "foreign project":
					target = foreignProject.base + "/work-items/" + string(f.issue.WorkItemID) + "/events"
				case "foreign organization":
					target = foreignOrganization.base + "/work-items/" + string(f.issue.WorkItemID) + "/events"
				case "repository":
					publication.Repository = "https://github.com/example-org/foreign-project"
					publication.External.URL = publication.Repository + "/pull/7"
				case "url":
					publication.External.URL = "https://attacker.example/pull/7"
				case "number":
					publication.External.ID = "8"
				case "base":
					publication.BaseRef = "other-base"
				case "branch":
					publication.Branch = "other-branch"
				case "source attempt":
					publication.SourceAttemptID = "attempt_foreign"
				case "source version":
					publication.SourceVersion.VersionID = "version_foreign"
				case "policy":
					publication.PolicyID = hubTestPolicy().ID
				case "lease":
					event.Data.LeaseID = "foreign-lease"
				case "fence":
					event.Data.FencingToken++
				case "attempt":
					event.Data.AttemptID = "attempt_foreign"
				case "run":
					event.Data.RunID = "run_foreign"
				}
				handoff.Change, handoff.HeadSHA = &publication.SourceVersion, publication.HeadSHA
				handoff.EffectID = publication.EffectID("pr_create")
				response := performHubAPIRequest(t, f.service, http.MethodPost, target, worker, event)
				if response.Code < 400 {
					t.Fatalf("mismatched publication acquired authority: %d %s", response.Code, response.Body.String())
				}
				if detail := f.detail(t); detail.Versions[0].External != nil || !reflect.DeepEqual(detail.Reviews, baseline.Reviews) {
					t.Fatal("rejected receipt changed linkage or review evidence")
				}
			})
		}
		t.Run("linked foreign item with its own lease", func(t *testing.T) {
			foreignLease := claimNativeAttempt(t, f.nativeFixture, worker, "linked-machine", "linked-session", foreignItem.WorkItemID, descriptor.ID)
			foreignStart := nativeStartedEvent(foreignLease)
			foreignPath := f.base + "/work-items/" + string(foreignItem.WorkItemID) + "/events"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, foreignPath, worker, foreignStart), http.StatusOK)
			foreignCheckpoint := foreignStart
			foreignCheckpoint.IdempotencyKey, foreignCheckpoint.Type, foreignCheckpoint.Data.Sequence = "linked-confirmed-effect", "run.checkpointed", 2
			foreignHandoff := *checkpoint.Data.Handoff
			foreignCheckpoint.Data.Handoff = &foreignHandoff
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, foreignPath, worker, foreignCheckpoint), http.StatusOK)
			foreignDiff := diff
			foreignDiff.Producer = tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: foreignStart.Data.AttemptID, LeaseID: foreignLease.ID, FencingToken: foreignLease.FencingToken}
			foreignDiff.Generation.Seq = 3
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/attempts/"+foreignStart.Data.AttemptID+"/diff", worker, foreignDiff), http.StatusAccepted)
			credential, _, err := f.service.authenticateAPIToken(t.Context(), worker, "", "")
			if err != nil {
				t.Fatal(err)
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(t.Context(), "INSERT INTO change_issue_links (change_id, organization_id, project_id, work_item_id) VALUES (?,?,?,?)", version.ChangeID, scope.organization, scope.project, foreignItem.WorkItemID); err != nil {
				t.Fatal(err)
			}
			foreignCheckpoint.IdempotencyKey, foreignCheckpoint.Data.Sequence = "linked-pr-receipt", 3
			foreignCheckpoint.Data.Handoff.Publication = &binding
			if err := validateNativeRunEvent(foreignCheckpoint); err != nil {
				t.Fatal(err)
			}
			foreignScope := scope
			foreignScope.credential = credential
			_, _, err = recordNativeAttempt(t.Context(), tx, foreignScope, foreignItem.WorkItemID, &foreignCheckpoint, f.service.config.now(), false)
			var refusal *nativeError
			if !errors.As(err, &refusal) || refusal.Code != "run_sequence_conflict" || !strings.Contains(refusal.Message, "PR publication no longer matches") {
				t.Fatalf("a linked item's independent owner acquired publication authority: %v", err)
			}
		})
		checkpoint.IdempotencyKey, checkpoint.Data.Sequence = "confirmed-pr-receipt", 4
		checkpoint.Data.Handoff.Publication = &binding
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, checkpoint), http.StatusOK)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, checkpoint), http.StatusOK)
		if result := readReferences(); !strings.Contains(string(result.Content), binding.External.URL) {
			t.Fatalf("work_references lacks confirmed PR before completion: %s", result.Content)
		}
		detail := f.detail(t)
		if detail.Versions[0].External == nil || *detail.Versions[0].External != binding.External || !reflect.DeepEqual(detail.Reviews, baseline.Reviews) || detail.Summary.NativeReview != baseline.Summary.NativeReview {
			t.Fatalf("early linkage changed source or review authority: %+v", detail)
		}
		var after string
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT record_json FROM change_versions WHERE id=?", version.ID).Scan(&after); err != nil || after != snapshot {
			t.Fatalf("early linkage rewrote immutable source: %v", err)
		}
		for _, sequence := range []int64{5, 6} {
			if sequence == 6 {
				request.IdempotencyKey, request.ExpectedVersionID = "second-pr-version", version.ID
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", worker, request)
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &version)
				binding.VersionID, binding.SourceVersion.VersionID = version.ID, version.ID
			}
			checkpoint.IdempotencyKey, checkpoint.Data.Sequence = fmt.Sprintf("retained-pr-%d", sequence), sequence
			checkpoint.Data.Handoff = nativeTestCheckpoint()
			if sequence == 6 {
				checkpoint.Data.Handoff.HeadSHA, checkpoint.Data.Handoff.Change = binding.HeadSHA, &binding.SourceVersion
				checkpoint.Data.Handoff.ExternalEffect, checkpoint.Data.Handoff.EffectState, checkpoint.Data.Handoff.EffectID = "pr_create", "confirmed", binding.EffectID("pr_create")
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, checkpoint), http.StatusOK)
		}
		diff.Generation.Seq = 7
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/attempts/"+start.Data.AttemptID+"/diff", worker, diff), http.StatusAccepted)
		checkpoint.IdempotencyKey, checkpoint.Data.Sequence = "second-pr-receipt", 7
		checkpoint.Data.Handoff.Publication = &binding
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, checkpoint), http.StatusOK)
		detail = f.detail(t)
		if len(detail.Versions) != 2 || detail.Versions[0].ID == detail.Versions[1].ID {
			t.Fatal("fixture must retain two distinct publication identities")
		}
		for _, version := range detail.Versions {
			if version.External == nil || *version.External != binding.External {
				t.Fatalf("publication lost historical or current binding: %+v", version)
			}
		}
		var status string
		var sequence int64
		var recorded string
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT status,sequence,data_json FROM native_attempts WHERE id=?", start.Data.AttemptID).Scan(&status, &sequence, &recorded); err != nil || status != "running" || sequence != 7 {
			t.Fatalf("early publication changed attempt completion: status=%s sequence=%d error=%v", status, sequence, err)
		}
		var run tracker.NativeRunData
		if err := decodeRunDetail(f.service.runDetailContext(t.Context()), recorded, &run); err != nil || run.Finalization != nil {
			t.Fatalf("early publication finalized the attempt: finalization=%+v error=%v", run.Finalization, err)
		}
		diff.Generation.Seq = 8
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/attempts/"+start.Data.AttemptID+"/diff", worker, diff), http.StatusAccepted)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
		checkpoint.IdempotencyKey, checkpoint.Data.Sequence = "released-pr-receipt", 8
		if response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, checkpoint); response.Code < 400 {
			t.Fatalf("released lease recorded publication: %d", response.Code)
		}
		credential, _, err := f.service.authenticateAPIToken(t.Context(), worker, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at=? WHERE id=?", formatHubTime(f.service.config.now()), credential.ID); err != nil {
			t.Fatal(err)
		}
		checkpoint.IdempotencyKey, checkpoint.Data.Sequence = "revoked-pr-receipt", 8
		if response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, checkpoint); response.Code != http.StatusUnauthorized {
			t.Fatalf("revoked owner recorded publication: %d", response.Code)
		}
	})
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
	descriptor  policy.Descriptor
	requirement providercapacity.Requirement
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
	bundle := changeTestSource(t, &input)
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
	claim := tracker.NativeClaim{LandingCapacity: capacity, PolicyID: f.descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: session, TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
	if f.requirement.Backend != "" {
		claim.ProviderCandidates = []tracker.NativeCapacityCandidate{{WorkItemID: issue.WorkItemID, Revision: issue.Revision, Requirement: f.requirement}}
	}
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
}

func TestGitHubLandingCapacityAcrossRunners(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                                                                                                                            string
		admission                                                                                                                       string
		ownerCapacity, contenderCapacity, renewalCapacity, replayCapacity, expiredRenewalCapacity                                       int
		boundary                                                                                                                        string
		provider, sprite, overflow                                                                                                      bool
		nativeOnly, sameBatch, otherProject, laneChanged, expired, concurrent, fallback, room, renewFree, renewRace, direct, directRace bool
	}{
		{name: "one project across hosts", ownerCapacity: 1, contenderCapacity: 1, admission: "skipped"},
		{name: "legacy owner occupies a slot", contenderCapacity: 1, admission: "unknown"},
		{name: "legacy contender cannot bypass owner ceiling", ownerCapacity: 1},
		{name: "wider contender preserves owner ceiling", ownerCapacity: 1, contenderCapacity: 3},
		{name: "lower contender retains its ceiling", ownerCapacity: 3, contenderCapacity: 1, admission: "unknown"},
		{name: "renewal tightens owner ceiling", ownerCapacity: 3, contenderCapacity: 3, renewalCapacity: 1},
		{name: "claim replay tightens owner ceiling", ownerCapacity: 3, contenderCapacity: 3, replayCapacity: 1},
		{name: "expired renewal applies incoming tighter ceiling", ownerCapacity: 3, contenderCapacity: 3, expired: true, expiredRenewalCapacity: 1},
		{name: "configured multiple slots permit capacity", ownerCapacity: 2, contenderCapacity: 2, room: true, admission: "unknown"},
		{name: "legacy unconfigured claims are unchanged", room: true, admission: "unknown"},
		{name: "lane change retains live occupancy", ownerCapacity: 1, contenderCapacity: 1, laneChanged: true},
		{name: "expired owner frees occupancy", ownerCapacity: 1, contenderCapacity: 1, expired: true, admission: "ready"},
		{name: "expired owner may renew a free slot", ownerCapacity: 1, contenderCapacity: 1, expired: true, renewFree: true},
		{name: "expired changed-lane owner cannot renew an occupied slot", ownerCapacity: 1, contenderCapacity: 1, expired: true, laneChanged: true},
		{name: "negative capacity is invalid", ownerCapacity: -1},
		{name: "separate project proceeds", ownerCapacity: 1, contenderCapacity: 1, otherProject: true, admission: "ready"},
		{name: "native-only landing is unchanged", ownerCapacity: 1, contenderCapacity: 1, nativeOnly: true, admission: "ready"},
		{name: "GitHub batch names retain independent occupancy", ownerCapacity: 1, contenderCapacity: 1, sameBatch: true},
		{name: "native batch shares its host slot", ownerCapacity: 1, contenderCapacity: 1, nativeOnly: true, sameBatch: true},
		{name: "native batch shares its provider reservation", ownerCapacity: 1, contenderCapacity: 1, nativeOnly: true, sameBatch: true, provider: true},
		{name: "native batch shares its Sprite slot", ownerCapacity: 1, contenderCapacity: 1, nativeOnly: true, sameBatch: true, sprite: true},
		{name: "native batch shares its Sprite overflow slot", ownerCapacity: 1, contenderCapacity: 1, nativeOnly: true, sameBatch: true, sprite: true, overflow: true},
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
			if test.sprite {
				owner.redemption.SpriteName = owner.redemption.Hostname
				placement := policy.Placement{Mode: "blended", MaxSpriteSlots: 1}
				if test.overflow {
					placement = policy.Placement{Mode: "local_first", TodoThreshold: 1, OverflowSlots: 1}
					f.createIndependent(t, "queued source")
				}
				setPlacementFixture(t, f.nativeFixture, placement, 1)
			}
			owner.enroll(t)
			if test.provider {
				f.requirement = providercapacity.Requirement{Role: "merge", Backend: "git", Model: "git"}
				report := capacityReport(f.service.config.now())
				report.Backend, report.Models = "git", []string{"git"}
				raw, err := json.Marshal([]providercapacity.Report{report})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET provider_reports_json=? WHERE id=?", string(raw), owner.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}
			waiter := prepareRunner(t, secondFixture.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Events)
			waiter.enroll(t)
			if test.overflow {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET state='disabled' WHERE id=?", waiter.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}
			ownerSession, waiterSession := "first-landing", "waiting-landing"
			if test.ownerCapacity < 0 {
				requireNativeStatus(t, f.claim(t, owner, first, ownerSession, test.ownerCapacity), http.StatusUnprocessableEntity)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims/preview", owner.redemption.Credential, tracker.NativeCapacityPreview{NativeClaim: tracker.NativeClaim{LandingCapacity: test.ownerCapacity, PolicyID: f.descriptor.ID, MachineID: owner.binding.MachineID}}), http.StatusUnprocessableEntity)
				return
			}
			if test.sameBatch {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, owner.identityPath()+"/routing", testHubAdminToken, map[string]any{"expected_revision": 1, "display_name": "Batch runner", "state": "active", "capacity_limit": 1, "project_ids": []tracker.ProjectID{f.project.ID}, "project_capacity_limits": map[tracker.ProjectID]int{f.project.ID: 1}, "idempotency_key": "batch-project-limit"}), http.StatusOK)
				waiter = owner
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET capacity_limit=1, reported_capacity=1 WHERE id=?", owner.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE machines SET capacity=1 WHERE id=?", owner.binding.MachineID); err != nil {
					t.Fatal(err)
				}
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
			if test.renewalCapacity > 0 {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", owner.redemption.Credential, tracker.NativeLeaseMutation{LandingCapacity: test.renewalCapacity, FencingToken: lease.FencingToken, TTLSeconds: 90}), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", owner.redemption.Credential, tracker.NativeLeaseMutation{LandingCapacity: test.ownerCapacity, FencingToken: lease.FencingToken, TTLSeconds: 90}), http.StatusOK)
			}
			if test.replayCapacity > 0 {
				requireNativeStatus(t, f.claim(t, owner, first, ownerSession, test.replayCapacity), http.StatusOK)
				requireNativeStatus(t, f.claim(t, owner, first, ownerSession, test.ownerCapacity), http.StatusOK)
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
				published, err := readChangeVersion(t.Context(), f.service.database.reader, change, version)
				if err != nil {
					t.Fatal(err)
				}
				event := nativeStartedEvent(direct)
				path := f.base + "/work-items/" + string(second.WorkItemID)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", waiter.redemption.Credential, event), http.StatusOK)
				event.Type, event.IdempotencyKey, event.Data.Sequence = "run.observed", "landing-wait", 2
				event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: f.service.config.now(), Landing: &tracker.NativeLandingReceipt{ChangeID: change, VersionID: version, HeadSHA: published.HeadSHA, Waiting: true}}
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
				if projected.ListChange == nil || !projected.ListChange.LandingPending || projected.ListChange.HeadSHA != published.HeadSHA {
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
			if test.sameBatch {
				if !test.nativeOnly {
					requireNativeStatus(t, f.claim(t, owner, second, waiterSession, 1), http.StatusConflict)
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", owner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
					requireNativeStatus(t, f.claim(t, owner, second, waiterSession, 1), http.StatusOK)
					return
				}
				if test.provider || test.sprite {
					response := f.claim(t, owner, second, waiterSession, 1)
					requireNativeStatus(t, response, http.StatusOK)
					var member tracker.NativeLease
					decodeHubResponse(t, response, &member)
					third := f.ready(t, "third batch member")
					response = f.claim(t, owner, third, "landing/shared/third", 1)
					requireNativeStatus(t, response, http.StatusOK)
					var last tracker.NativeLease
					decodeHubResponse(t, response, &last)
					fourth := f.ready(t, "different batch")
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE machines SET capacity=4 WHERE id=?", owner.binding.MachineID); err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET capacity_limit=4, reported_capacity=4 WHERE id=?", owner.binding.RunnerID); err != nil {
						t.Fatal(err)
					}
					other := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim)
					other.redemption.SpriteName = owner.redemption.SpriteName
					other.enroll(t)
					independent := newLandingCapacityFixture(t, f.service, "independent-slots", false)
					independent.requirement = f.requirement
					if test.overflow {
						independent.createIndependent(t, "independent source")
					}
					otherItem := independent.ready(t, "independent landing")
					projectRunner := prepareRunner(t, independent.nativeFixture, runnerauth.Read, runnerauth.Claim)
					projectRunner.redemption.SpriteName = owner.redemption.SpriteName
					projectRunner.enroll(t)
					if test.provider {
						var reports string
						if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT provider_reports_json FROM runner_identities WHERE id=?", owner.binding.RunnerID).Scan(&reports); err != nil {
							t.Fatal(err)
						}
						for _, r := range []runnerFixture{other, projectRunner} {
							if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET provider_reports_json=? WHERE id=?", reports, r.binding.RunnerID); err != nil {
								t.Fatal(err)
							}
						}
					}
					for _, active := range []tracker.NativeLease{lease, member, last} {
						requireNativeStatus(t, f.claim(t, other, fourth, "landing/shared/other", 1), http.StatusConflict)
						requireNativeStatus(t, independent.claim(t, projectRunner, otherItem, "landing/shared/project", 1), http.StatusConflict)
						requireNativeStatus(t, f.claim(t, owner, fourth, "landing/different/owner", 1), http.StatusConflict)
						if test.sprite {
							used, err := readPlacementSpriteUsage(t.Context(), f.service.database.reader, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, f.service.config.now())
							if err != nil || used != 1 {
								t.Fatalf("Sprite usage=%d, err=%v, want 1", used, err)
							}
						}
						if test.provider {
							report := capacityReport(f.service.config.now())
							report.Backend, report.Models = "git", []string{"git"}
							view, err := providerView(t.Context(), f.service.database.reader, f.project.OrganizationID, owner.binding.RunnerID, report, f.service.config.now())
							if err != nil || view.Used != 1 {
								t.Fatalf("provider usage=%d, err=%v, want 1", view.Used, err)
							}
						}
						requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(active.ID)+"/renew", owner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: active.FencingToken, TTLSeconds: 90}), http.StatusOK)
						requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(active.ID)+"/release", owner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: active.FencingToken, Reason: "completed"}), http.StatusNoContent)
					}
					response = independent.claim(t, projectRunner, otherItem, "landing/shared/project", 1)
					requireNativeStatus(t, response, http.StatusOK)
					var independentLease tracker.NativeLease
					decodeHubResponse(t, response, &independentLease)
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, independent.base+"/leases/"+string(independentLease.ID)+"/release", projectRunner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: independentLease.FencingToken, Reason: "completed"}), http.StatusNoContent)
					requireNativeStatus(t, f.claim(t, other, fourth, "landing/different/owner", 1), http.StatusOK)
					return
				}
				for _, exclusion := range []struct {
					name, change, restore, code string
					args                        []any
				}{
					{"draining", "UPDATE runner_identities SET state='draining' WHERE id=?", "UPDATE runner_identities SET state='active' WHERE id=?", "runner_draining", []any{owner.binding.RunnerID}},
					{"disabled", "UPDATE runner_identities SET state='disabled' WHERE id=?", "UPDATE runner_identities SET state='active' WHERE id=?", "runner_disabled", []any{owner.binding.RunnerID}},
					{"offline", "UPDATE runner_identities SET last_heartbeat_at='2000-01-01T00:00:00Z' WHERE id=?", "UPDATE runner_identities SET last_heartbeat_at=? WHERE id=?", "runner_offline", []any{owner.binding.RunnerID}},
					{"runner paused", "UPDATE runner_identities SET capacity_limit=0 WHERE id=?", "UPDATE runner_identities SET capacity_limit=1 WHERE id=?", "runner_capacity", []any{owner.binding.RunnerID}},
					{"host paused", "UPDATE machines SET capacity=0 WHERE id=?", "UPDATE machines SET capacity=1 WHERE id=?", "host_capacity", []any{owner.binding.MachineID}},
					{"provider exhausted", `UPDATE runner_identities SET provider_reports_json=? WHERE id=?`, `UPDATE runner_identities SET provider_reports_json='[]' WHERE id=?`, "provider_capacity", nil},
				} {
					t.Run(exclusion.name, func(t *testing.T) {
						args := exclusion.args
						if exclusion.name == "provider exhausted" {
							report := capacityReport(f.service.config.now())
							report.Availability = "exhausted"
							raw, err := json.Marshal([]providercapacity.Report{report})
							if err != nil {
								t.Fatal(err)
							}
							args = []any{string(raw), owner.binding.RunnerID}
						}
						if _, err := f.service.database.db.ExecContext(t.Context(), exclusion.change, args...); err != nil {
							t.Fatal(err)
						}
						response := f.claim(t, owner, second, waiterSession, test.contenderCapacity)
						requireNativeStatus(t, response, http.StatusConflict)
						var refusal nativeError
						decodeHubResponse(t, response, &refusal)
						if refusal.Code != exclusion.code {
							t.Fatalf("refusal=%s, want %s", refusal.Code, exclusion.code)
						}
						if exclusion.name == "offline" {
							args = []any{formatHubTime(f.service.config.now()), owner.binding.RunnerID}
						}
						if exclusion.name == "provider exhausted" {
							args = []any{owner.binding.RunnerID}
						}
						if _, err := f.service.database.db.ExecContext(t.Context(), exclusion.restore, args...); err != nil {
							t.Fatal(err)
						}
					})
				}
				response := f.claim(t, owner, second, waiterSession, test.contenderCapacity)
				requireNativeStatus(t, response, http.StatusOK)
				var member tracker.NativeLease
				decodeHubResponse(t, response, &member)
				if member.ID == lease.ID || member.FencingToken <= lease.FencingToken {
					t.Fatal("batch member did not acquire its own fenced lease")
				}
				tx, err := f.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				available, err := machineClaimCapacity(t.Context(), tx, owner.binding.MachineID, f.service.config.now(), "", "")
				if rollbackErr := tx.Rollback(); err != nil || rollbackErr != nil {
					t.Fatal(err, rollbackErr)
				}
				if available != 0 {
					t.Fatalf("two native batch owners consumed more than one host slot: available=%d, want0", available)
				}
				runner, err := readRunner(t.Context(), f.service.database.db, f.project.OrganizationID, owner.binding.RunnerID, f.service.config.now())
				if err != nil || runner.Used != 1 || runner.ProjectUsed[f.project.ID] != 1 || len(runner.Leases) != 2 {
					t.Fatalf("native batch project accounting=%+v error=%v", runner, err)
				}
				third := f.ready(t, "different batch")
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE machines SET capacity=2 WHERE id=?", owner.binding.MachineID); err != nil {
					t.Fatal(err)
				}
				response = f.claim(t, owner, third, "landing/different/owner", 1)
				requireNativeStatus(t, response, http.StatusConflict)
				var refusal nativeError
				decodeHubResponse(t, response, &refusal)
				if refusal.Code != "runner_capacity" {
					t.Fatalf("different batch refusal=%s, want runner_capacity", refusal.Code)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE machines SET capacity=1 WHERE id=?", owner.binding.MachineID); err != nil {
					t.Fatal(err)
				}
				requireNativeStatus(t, f.claim(t, owner, third, "landing/different/owner", 1), http.StatusConflict)
				other := sharedRunner(t, owner)
				other.redemption.BackendIsolation = owner.redemption.BackendIsolation
				publishCapacity(t, f.nativeFixture, other)
				requireNativeStatus(t, f.claim(t, other, third, "landing/shared/other", 1), http.StatusConflict)
				independent := newLandingCapacityFixture(t, f.service, "independent-batch", false)
				fourth := independent.ready(t, "independent change")
				projectRunner := sharedRunner(t, runnerFixture{nativeFixture: independent.nativeFixture, binding: owner.binding, base: owner.base})
				projectRunner.redemption.BackendIsolation = owner.redemption.BackendIsolation
				publishCapacity(t, independent.nativeFixture, projectRunner)
				requireNativeStatus(t, independent.claim(t, owner, fourth, "landing/shared/project", 1), http.StatusNotFound)
				requireNativeStatus(t, independent.claim(t, projectRunner, fourth, "landing/shared/project", 1), http.StatusConflict)
				for _, active := range []tracker.NativeLease{lease, member} {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(active.ID)+"/renew", owner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: active.FencingToken, TTLSeconds: 90}), http.StatusOK)
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(active.ID)+"/release", owner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: active.FencingToken, Reason: "completed"}), http.StatusNoContent)
					if active.ID == lease.ID {
						requireNativeStatus(t, f.claim(t, other, third, "landing/shared/other", 1), http.StatusConflict)
					}
				}
				response = independent.claim(t, projectRunner, fourth, "landing/shared/project", 1)
				requireNativeStatus(t, response, http.StatusOK)
				var independentLease tracker.NativeLease
				decodeHubResponse(t, response, &independentLease)
				requireNativeStatus(t, performHubAPIRequest(t, independent.service, http.MethodPost, independent.base+"/leases/"+string(independentLease.ID)+"/release", projectRunner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: independentLease.FencingToken, Reason: "completed"}), http.StatusNoContent)
				requireNativeStatus(t, f.claim(t, other, third, "landing/shared/other", 1), http.StatusOK)
				return
			}
			want := http.StatusConflict
			if test.nativeOnly || test.otherProject || test.expired && !test.renewFree || test.room {
				want = http.StatusOK
			}
			preview := performHubAPIRequest(t, f.service, http.MethodPost, secondFixture.base+"/claims/preview", waiter.redemption.Credential, tracker.NativeCapacityPreview{NativeClaim: tracker.NativeClaim{LandingCapacity: test.contenderCapacity, PolicyID: secondFixture.descriptor.ID, MachineID: waiter.binding.MachineID}})
			if test.sameBatch {
				requireNativeStatus(t, preview, http.StatusConflict)
			} else {
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
			}
			if test.admission != "" {
				context := tracker.NativeAdmissionContext{ObservedAt: f.service.config.now(), PolicyID: secondFixture.descriptor.ID, WorkflowStates: []string{"Merging"}}
				raw, err := json.Marshal(context)
				if err != nil {
					t.Fatal(err)
				}
				path := secondFixture.base + "/work-items/" + string(second.WorkItemID) + "/runtime?admission=" + url.QueryEscape(string(raw))
				response := performHubAPIRequest(t, f.service, http.MethodGet, path, waiter.redemption.Credential, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var evidence tracker.NativeRuntimeEvidence
				decodeHubResponse(t, response, &evidence)
				if evidence.Scheduling.Outcome != test.admission {
					t.Fatalf("runtime scheduling claims an unproved ceiling: %#v", evidence.Scheduling)
				}
				found := false
				for _, admission := range evidence.Admission {
					if admission.RunnerID != waiter.binding.RunnerID {
						continue
					}
					found = true
					if admission.Outcome != test.admission || slices.Contains(admission.Unavailable, "project_landing_capacity") != (test.admission == "unknown") {
						t.Fatalf("runtime admission invents incoming capacity: %#v", admission)
					}
				}
				if !found {
					t.Fatal("runtime lost the contender's admission evidence")
				}
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
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", owner.redemption.Credential, tracker.NativeLeaseMutation{LandingCapacity: test.expiredRenewalCapacity, FencingToken: lease.FencingToken, TTLSeconds: 90}), http.StatusConflict)
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
	updated.Gates.LandingMode = "per_landing"
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
		{name: "operator hold stays", states: []tracker.NativeState{
			{Name: "Blocked", Transitions: []string{"In Progress", "Merging"}},
			{Name: "In Progress", Dispatchable: true, Transitions: []string{"Blocked"}},
			{Name: "Merging", Dispatchable: true, Transitions: []string{"Done"}},
			{Name: "Done", Terminal: true},
		}, from: "Blocked", want: "Blocked"},
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
					if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
						t.Fatal(err)
					}
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
		{name: "an operator hold stays where it is", from: "Blocked", want: "Blocked", wantComment: true},
		{name: "an intake item stays where it is", from: "Backlog", want: "Backlog", wantComment: true},
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
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "work"}, Title: "Rework me", Body: issueContractTestSections, State: test.from})
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

func TestReviewConversationHistoryDoesNotVetoLanding(t *testing.T) {
	t.Parallel()
	refusal := "GitHub requires these review conversations to be resolved before the merge. Address each finding in Rework, then resolve its conversation with evidence of the fix or dismissal.\n- review-bot on internal/app.go:12: handle the empty case"
	for _, test := range []struct {
		name     string
		detail   string
		reentry  string
		publish  bool
		resolved bool
		want     bool
	}{
		{name: "runner re-entry recomputes readiness", detail: refusal, reentry: "runner", want: true},
		{name: "a human move into Merging releases the refusal", detail: refusal, reentry: "human", want: true},
		{name: "a newly published version releases the refusal", detail: refusal, reentry: "runner", publish: true, want: true},
		{name: "other landing refusals keep their retry path", detail: "GitHub refused the pull request operation: Pull Request is still a draft", reentry: "runner", want: true},
		{name: "a Rework that resolved the conversations releases the refusal", detail: refusal, reentry: "runner", resolved: true, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newLandingCapacityFixture(t, nil, "review-refusal", true)
			issue := f.ready(t, "refused change")
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			ready := func() bool {
				t.Helper()
				tx, err := f.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				var id tracker.WorkItemID
				if err := tx.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE native_id = ?", issue.WorkItemID).Scan(&id); err != nil {
					t.Fatal(err)
				}
				ready, evaluated, _, err := nativeLandingCandidateReady(t.Context(), tx, &scope, id, "machine_landing", 0, f.service.config.now())
				if err != nil || !evaluated {
					t.Fatalf("landing eligibility evaluated = %t, error = %v", evaluated, err)
				}
				return ready
			}
			if !ready() {
				t.Fatal("reviewed version was not ready before any refusal")
			}
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/workflow"
			move := func(state, detail string) {
				t.Helper()
				current, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(issue.WorkItemID))
				if err != nil {
					t.Fatal(err)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "move-" + state}, ExpectedRevision: current.Revision, State: state, Reason: "user_requested", ReasonDetail: detail}), http.StatusOK)
			}
			move("In Progress", test.detail)
			var recovery tracker.NativeRecovery
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path[:len(path)-len("/workflow")]+"?view=recovery", f.token, nil), &recovery)
			if len(recovery.History) != 1 || recovery.History[0].Data.FromState != "Merging" || recovery.History[0].Data.ReasonDetail != test.detail {
				t.Fatalf("recovery omitted the landing exit: %+v", recovery.History)
			}
			if test.reentry == "human" {
				move("Merging", "")
			} else {
				reentryDetail := "The run succeeded and opened Change Request change_1. The runner lands it next."
				if test.resolved {
					reentryDetail += " " + workspace.ReviewConversationsResolvedText
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='Merging') WHERE native_id=?", f.project.ID, issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO collaboration_events (id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at) SELECT 'runner-reentry',organization_id,project_id,work_item_id,sequence+1,'workflow.transitioned',1,'{"kind":"runner","principal_id":"runner_landing"}',json_object('from_state','In Progress','to_state','Merging','reason','worker_progress','reason_detail',?),recorded_at FROM collaboration_events WHERE work_item_id=? ORDER BY sequence DESC LIMIT 1`, reentryDetail, issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			var actor string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT json_extract(actor_json,'$.kind') FROM collaboration_events WHERE work_item_id=? AND type='workflow.transitioned' ORDER BY sequence DESC LIMIT 1", issue.WorkItemID).Scan(&actor); err != nil || actor != test.reentry {
				t.Fatalf("re-entry actor = %q, error = %v, want %s", actor, err, test.reentry)
			}
			if test.publish {
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO collaboration_events (id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at) SELECT 'republished',organization_id,project_id,work_item_id,sequence+1,'change.version_published',1,actor_json,'{}',recorded_at FROM collaboration_events WHERE work_item_id=? ORDER BY sequence DESC LIMIT 1`, issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			if got := ready(); got != test.want {
				t.Fatalf("landing ready = %t, want %t", got, test.want)
			}
		})
	}
}
