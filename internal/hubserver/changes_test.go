package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
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

func newChangeFixture(t *testing.T, service *Service, organizations ...tracker.OrganizationID) changeFixture {
	t.Helper()
	return newNamedChangeFixture(t, service, "changes", organizations...)
}

func newNamedChangeFixture(t *testing.T, service *Service, name string, organizations ...tracker.OrganizationID) changeFixture {
	t.Helper()
	organization := tracker.OrganizationID("")
	if len(organizations) > 0 {
		organization = organizations[0]
	}
	f := newNativeFixture(t, service, organization, name)
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil {
		t.Fatal(err)
	}
	principal := credential.ID
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
	current, err := f.service.selectedCurrentChangeSummary(t.Context(), hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID}, f.issue.WorkItemID, detail.Change.ID, f.service.config.Clock())
	if err != nil || !reflect.DeepEqual(current, detail.Summary) {
		t.Fatalf("current summary = %#v, full summary = %#v, error = %v", current, detail.Summary, err)
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
			t.Parallel()
			f := newChangeFixture(t, nil)
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
				rules := f.rules
				rules.PolicyID = descriptor.ID
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "aggregate-rules"}, ExpectedID: f.rules.ID, Policy: rules}), http.StatusOK)
				if test.aggregatePressure {
					body := f.issue.Body + strings.Repeat("<", (64<<10)-len(f.issue.Body))
					if test.fragmentBody {
						body = f.issue.Body + strings.Repeat("<", 153<<10) + strings.Repeat("b", (256<<10)-len(f.issue.Body)-(153<<10))
					}
					update := tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "aggregate-body"}, ExpectedRevision: f.issue.Revision, Body: &body}
					encoded, err := json.Marshal(update)
					if err != nil || len(encoded) >= hubapi.MaxAPIRequestBodyBytes {
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
				result := &gate.ValidatorResult{SessionID: 42, VersionID: version.ID, Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, Submitted: true, Verdict: verdict, Attribution: "unknown", Commands: []gate.CommandResult{{Command: "make check-land", Output: strings.Repeat("x", bytes), Attribution: "unknown"}}}
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
			evidence := make([]eventstore.Evidence, 0, len(history))
			content := eventstore.NewContent(f.service.runDetailStore())
			for _, review := range history {
				raw, err := json.Marshal(review)
				if err != nil {
					t.Fatal(err)
				}
				row, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: f.project.OrganizationID, ChangeID: f.change.ID, Kind: "review"}, raw)
				if err != nil {
					t.Fatal(err)
				}
				evidence = append(evidence, row)
			}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				for _, row := range evidence {
					if _, err := w.AppendEvidence(t.Context(), row); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
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
					t.Fatalf("bounded review changed feedback identity, body or decision: got id=%s version=%s decision=%s actor=%+v created=%s body=%d bytes, want id=%s version=%s decision=%s actor=%+v created=%s body=%d bytes", review.ID, review.VersionID, review.Decision, review.Actor, review.CreatedAt, len(review.Body), original.ID, original.VersionID, original.Decision, original.Actor, original.CreatedAt, len(original.Body))
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
	config := Config{Clock: func() time.Time { return now }}
	f := newChangeFixture(t, newPostgresNativeService(t, config))
	if detail := f.detail(t); detail.Summary.Status != "draft" || len(detail.Versions) != 0 {
		t.Fatalf("draft = %#v", detail)
	}
	input := changeTestInput()
	legacyVisual := tracker.ChangeArtifact{Kind: "artifact", URI: "https://example.test/screenshot.png", SHA256: policy.Digest([]byte("image")), Availability: "available"}
	input.Artifacts = append(input.Artifacts, legacyVisual)
	input.ReviewDispositions = []tracker.ReviewDisposition{
		{HeadSHA: input.HeadSHA, ThreadID: "T1", Disposition: "fixed", Evidence: "source.go:17 and regression passes"},
		{HeadSHA: input.HeadSHA, ThreadID: "T2", Disposition: "dismissed", Evidence: "source.go:29 already checks the head"},
	}
	request := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "v1"}, ChangeVersionInput: input}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, request)
	requireNativeStatus(t, response, http.StatusOK)
	var first tracker.ChangeVersion
	decodeHubResponse(t, response, &first)
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, request)
	requireNativeStatus(t, response, http.StatusOK)
	var duplicate tracker.ChangeVersion
	decodeHubResponse(t, response, &duplicate)
	if duplicate.ID != first.ID {
		t.Fatal("retry changed immutable identity")
	}
	path := f.path + "/versions/" + first.ID
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "approval"}, Decision: "approved", Body: "https://figma.com/design/example"}), http.StatusOK)
	checkRequest := changeTestResult(first)
	checkRequest.Evidence = append(checkRequest.Evidence, legacyVisual)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/checks", f.token, checkRequest), http.StatusOK)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		return w.UpdateProjectContent(t.Context(), f.project.ID, record.Core.ContentSettingsRevision, true)
	}); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, request), http.StatusOK)
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
	late := checkRequest
	late.IdempotencyKey = "late-replay"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/checks", f.token, late), http.StatusOK)
	for _, statement := range []string{"UPDATE change_versions SET number = 10 WHERE organization_id=$1 AND change_id=$2", "DELETE FROM change_versions WHERE organization_id=$1 AND change_id=$2", "UPDATE change_evidence SET kind = 'review' WHERE organization_id=$1 AND change_id=$2", "DELETE FROM change_evidence WHERE organization_id=$1 AND change_id=$2"} {
		_, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), statement, f.project.OrganizationID, f.change.ID)
		if err == nil {
			t.Fatalf("immutable storage accepted %s", statement)
		}
		var refusal *pgconn.PgError
		if !errors.As(err, &refusal) || refusal.Code != "23514" {
			t.Fatalf("immutable storage failed without its integrity refusal: %v", err)
		}
	}
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = reopenPostgresNativeService(t, f.service)
	detail := f.detail(t)
	if detail.Change.ID != f.change.ID || detail.Change.CurrentVersion != second.ID || len(detail.Versions) != 2 || detail.Summary.NativeReview != "stale" || detail.Summary.Checks != "missing" || len(detail.Checks) != 1 {
		t.Fatalf("restarted version history = %#v", detail)
	}
	if !reflect.DeepEqual(detail.Versions[0].Artifacts, input.Artifacts) || !reflect.DeepEqual(detail.Checks[0].Evidence, checkRequest.Evidence) {
		t.Fatal("policy toggle changed historical visual evidence")
	}
	if !reflect.DeepEqual(detail.Versions[0].ReviewDispositions, input.ReviewDispositions) || len(detail.Versions[1].ReviewDispositions) != 0 {
		t.Fatalf("review dispositions lost their immutable version binding: %+v", detail.Versions)
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
		{"run", func(r *tracker.SubmitChangeCheck) { r.RunID = hubapi.NewID("run") }},
		{"check run", func(r *tracker.SubmitChangeCheck) { r.CheckRunID = hubapi.NewID("check") }},
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
	fixture := coreWorkItemNeutralFixtureBody(t, f.project.OrganizationID, f.project.ID, false, issueContractTestSections+"\n"+strings.Repeat("x", 8192))
	fixture.Set("repositories", map[string]string{"id": "1"}, map[string]any{"github_owner": "digitaldrywood", "github_name": "detent", "github_node_id": "R_original"})
	fixture.Set("issues", map[string]string{"id": "41"}, map[string]any{"number": 100, "github_node_id": "I_original", "github_number": 3410, "url": "https://github.com/wrong/repo/issues/29", "source_updated_at": testTimestamp, "synchronized_at": testTimestamp})
	fixture.Set("issues", map[string]string{"id": "43"}, map[string]any{"number": 102})
	original := fixture.Write()
	if err := neutralimport.Read(t.Context(), original, f.project.OrganizationID, func(bundle *neutralimport.Bundle) error {
		source := coreimport.CoreWorkItemImportSource{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, Objects: f.service.runDetailStore()}
		reader := neutralimport.NewIndex(bundle)
		for _, item := range []struct {
			id     string
			number int
		}{{"wi_core_import", 100}, {"wi_core_import_second", 102}} {
			for revision := int64(1); revision <= 2; revision++ {
				raw, err := fixtureCollaborationVersion(t.Context(), reader, string(source.OrganizationID), string(source.ProjectID), item.id, revision)
				if err != nil {
					return err
				}
				var issue tracker.NativeIssue
				if err := json.Unmarshal(raw, &issue); err != nil {
					return err
				}
				issue.Number = item.number
				raw, err = json.Marshal(issue)
				if err != nil {
					return err
				}
				fixture.Set("collaboration_versions", map[string]string{"record_id": item.id, "revision": fmt.Sprint(revision)}, map[string]any{"encoding": "snapshot", "record_json": string(raw)})
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	source := coreimport.CoreWorkItemImportSource{BundlePath: fixture.Write(), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, Objects: f.service.runDetailStore()}
	for range 2 {
		if count, err := coreimport.ImportCoreWorkItems(t.Context(), f.service.database.postgres, source); err != nil || count != 2 {
			t.Fatalf("verified issue import = %d, error = %v", count, err)
		}
	}
	f.issue = readWorkItem(t, f.nativeFixture, "wi_core_import", "")
	f.path = f.base + "/work-items/" + string(f.issue.WorkItemID) + "/changes"
	created := performHubAPIRequest(t, f.service, http.MethodPost, f.path, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "imported-change"}, Title: "Imported change"})
	requireNativeStatus(t, created, http.StatusOK)
	decodeHubResponse(t, created, &f.change)
	f.path += "/" + f.change.ID
	migrated := readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, "")
	wantImported := tracker.GitHubIssueSourceReference("I_original", "https://github.com/digitaldrywood/detent/issues/3410")
	if migrated.Number == 3410 || !reflect.DeepEqual(migrated.ExternalReferences, []tracker.ExternalReference{wantImported}) {
		t.Fatalf("migration lost source identity: %+v", migrated)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		project.Settings.Repository, project.Settings.CheckoutRepository, project.Settings.RepositoryEnabled = "digitaldrywood/detent", "digitaldrywood/detent", true
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
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
	event.Data.Identity.Role, event.Data.MachineID = "code", lease.MachineID
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", worker, event), http.StatusOK)
	input := changeTestInput()
	input.RunID, input.AttemptID = event.Data.RunID, event.Data.AttemptID
	bundle := changeTestSource(t, &input)
	diff := tracker.AttemptDiffRequest{Producer: tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: event.Data.AttemptID, LeaseID: lease.ID, FencingToken: lease.FencingToken}, Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 1}, BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/attempts/"+event.Data.AttemptID+"/diff", worker, diff), http.StatusAccepted)
	for _, test := range []struct {
		name string
		edit func(*tracker.PublishChangeVersion)
		want int
	}{
		{"missing fence", func(r *tracker.PublishChangeVersion) { r.LeaseID = "" }, http.StatusConflict},
		{"stale fence", func(r *tracker.PublishChangeVersion) { r.FencingToken++ }, http.StatusConflict},
		{"stale fence before invalid source", func(r *tracker.PublishChangeVersion) { r.FencingToken++; r.SourceBundle = []byte("invalid pack") }, http.StatusConflict},
		{"unrelated run", func(r *tracker.PublishChangeVersion) { r.RunID = hubapi.NewID("run") }, http.StatusNotFound},
		{"missing run", func(r *tracker.PublishChangeVersion) { r.RunID, r.AttemptID = "", "" }, http.StatusUnprocessableEntity},
		{"changed source bytes", func(r *tracker.PublishChangeVersion) { r.SourceBundle = []byte("other source") }, http.StatusUnprocessableEntity},
		{"wrong source base", func(r *tracker.PublishChangeVersion) { r.Source.BaseSHA = r.HeadSHA }, http.StatusUnprocessableEntity},
		{"wrong source head", func(r *tracker.PublishChangeVersion) { r.Source.HeadSHA = r.BaseSHA }, http.StatusUnprocessableEntity},
		{"missing source metadata", func(r *tracker.PublishChangeVersion) { r.Source = nil }, http.StatusUnprocessableEntity},
		{"current", func(_ *tracker.PublishChangeVersion) {}, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: test.name, LeaseID: lease.ID, FencingToken: lease.FencingToken}, ChangeVersionInput: input, SourceBundle: bundle}
			source := *input.Source
			request.Source = &source
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
	t.Run("compacted history", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Microsecond)
		service := newClockedPostgresNativeService(t, Config{Clock: func() time.Time { return now }})
		for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
			t.Run(string(organization), func(t *testing.T) {
				f := newChangeFixture(t, service, organization)
				descriptor := hubTestPolicy()
				descriptor.Gates.GitHubPullRequest = true
				descriptor = descriptor.WithID()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: hubTestPolicy().ID, Policy: descriptor}), http.StatusOK)
				rules := f.rules
				rules.PolicyID = descriptor.ID
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "push-policy"}, ExpectedID: f.rules.ID, Policy: rules}), http.StatusOK)
				worker := f.worker(t, "retained-push-worker")
				lease := claimNativeAttempt(t, f.nativeFixture, worker, "retained-machine", "retained-session", f.issue.WorkItemID, descriptor.ID)
				start := nativeStartedEvent(lease)
				start.Data.Identity.Role, start.Data.MachineID = "code", lease.MachineID
				path := f.base + "/work-items/" + string(f.issue.WorkItemID)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, start), http.StatusOK)
				input := changeTestInput()
				input.PolicyID, input.RunID, input.AttemptID = descriptor.ID, start.Data.RunID, start.Data.AttemptID
				bundle := changeTestSource(t, &input)
				diff := tracker.AttemptDiffRequest{Producer: tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: start.Data.AttemptID, LeaseID: lease.ID, FencingToken: lease.FencingToken}, Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 1}, BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/attempts/"+start.Data.AttemptID+"/diff", worker, diff), http.StatusAccepted)
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", worker, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "push-source", LeaseID: lease.ID, FencingToken: lease.FencingToken}, ChangeVersionInput: input, SourceBundle: bundle})
				requireNativeStatus(t, response, http.StatusOK)
				var version tracker.ChangeVersion
				decodeHubResponse(t, response, &version)
				reference := tracker.NativeChangeReference{ChangeID: version.ChangeID, VersionID: version.ID, HeadSHA: version.HeadSHA}
				binding := tracker.NativePRPublication{ChangeID: version.ChangeID, VersionID: version.ID, Repository: version.Repository, HeadSHA: version.HeadSHA, PolicyID: version.PolicyID, BaseRef: "main", Branch: "retained", SourceVersion: reference, SourceAttemptID: version.AttemptID}
				checkpoint := start
				checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Sequence = "run.checkpointed", "retained-confirmed-push", 2
				checkpoint.Data.Handoff = nativeTestCheckpoint()
				checkpoint.Data.Handoff.Change, checkpoint.Data.Handoff.HeadSHA = &reference, version.HeadSHA
				checkpoint.Data.Handoff.ExternalEffect, checkpoint.Data.Handoff.EffectState, checkpoint.Data.Handoff.EffectID = "git_push", "confirmed", binding.EffectID("git_push")
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, checkpoint), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
				scope := nativeAttemptListingScope(t, f.nativeFixture)
				baseline := f.detail(t)
				var original eventstore.Version
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					original, err = r.Version(t.Context(), version.ChangeID, version.ID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				content := eventstore.NewContent(f.service.runDetailStore())
				originalPayload, err := content.Version(t.Context(), original)
				if err != nil {
					t.Fatal(err)
				}
				for range 2 {
					if err := f.service.maintainNativeRetention(t.Context(), now.Add(31*24*time.Hour)); err != nil {
						t.Fatal(err)
					}
					pushes, err := f.service.readBoundedChangePushes(t.Context(), scope, string(f.issue.WorkItemID), []tracker.ChangeVersion{version})
					if err != nil || len(pushes) != 1 || pushes[0].Change != reference || pushes[0].RecordingAttemptID != start.Data.AttemptID || pushes[0].EffectID != binding.EffectID("git_push") {
						t.Fatalf("retained checkpoint attribution changed after release: %+v error=%v", pushes, err)
					}
					after := f.detail(t)
					if !reflect.DeepEqual(after.Versions, baseline.Versions) || !reflect.DeepEqual(after.Reviews, baseline.Reviews) || !reflect.DeepEqual(after.Summary, baseline.Summary) || after.External != nil {
						t.Fatalf("push proof mutated source or review evidence: detail=%+v", after)
					}
					var current eventstore.Version
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						current, err = r.Version(t.Context(), version.ChangeID, version.ID)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					currentPayload, err := content.Version(t.Context(), current)
					if err != nil || !reflect.DeepEqual(current, original) || !reflect.DeepEqual(currentPayload, originalPayload) {
						t.Fatalf("immutable source snapshot changed: error=%v", err)
					}
				}
			})
		}
	})

	now := time.Date(2026, time.October, 25, 12, 0, 0, 0, time.UTC)
	details := &memoryRunDetails{}
	service := newClockedPostgresNativeService(t, Config{Clock: func() time.Time { return now }, RunDetails: details})
	f := newChangeFixture(t, service)
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
	start.Data.Identity.Role, start.Data.MachineID = "code", lease.MachineID
	path := f.base + "/work-items/" + string(f.issue.WorkItemID)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, start), http.StatusOK)
	input := changeTestInput()
	input.PolicyID, input.RunID, input.AttemptID = descriptor.ID, start.Data.RunID, start.Data.AttemptID
	bundle := changeTestSource(t, &input)
	initialDiff := tracker.AttemptDiffRequest{Producer: tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: start.Data.AttemptID, LeaseID: lease.ID, FencingToken: lease.FencingToken}, Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 1}, BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/attempts/"+start.Data.AttemptID+"/diff", worker, initialDiff), http.StatusAccepted)
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
	scope := nativeAttemptListingScope(t, f.nativeFixture)
	foreignProject := newNativeFixture(t, f.service, f.project.OrganizationID, "other-push-project")
	foreignOrganization := newNativeFixture(t, f.service, "org_first_second", "other-push-organization")
	foreignProject.create(t, "foreign project work")
	foreignOrganization.create(t, "foreign organization work")
	foreignItem := f.create(t, "foreign work")
	baseline := f.detail(t)
	sourceSnapshot := func(t *testing.T, fixture changeFixture, version tracker.ChangeVersion) (eventstore.Version, tracker.ChangeVersion) {
		t.Helper()
		var row eventstore.Version
		if err := service.config.TenantRepository.ReadTenant(t.Context(), fixture.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			row, err = r.Version(t.Context(), version.ChangeID, version.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		payload, err := eventstore.NewContent(service.runDetailStore()).Version(t.Context(), row)
		if err != nil {
			t.Fatal(err)
		}
		return row, payload
	}
	snapshot, snapshotPayload := sourceSnapshot(t, f, version)
	verifyHistoricalAttempt := func(t *testing.T, local changeFixture, id string, includePolicy bool) {
		t.Helper()
		scope := nativeAttemptListingScope(t, local.nativeFixture)
		snapshot, err := service.readNativeAttemptSnapshotTenant(t.Context(), scope, string(local.issue.WorkItemID), id)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := service.readNativeAttemptRunTenant(t.Context(), snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if actual.AttemptID != id || actual.RunID != snapshot.Attempt.RunID || actual.LeaseID != tracker.LeaseID(snapshot.Lease.LeaseID) || actual.FencingToken != tracker.FencingToken(snapshot.Lease.FencingToken) || actual.Sequence != snapshot.Attempt.Sequence {
			t.Fatal("historical attempt fixture lost independent identity")
		}
		if snapshot.Payload == nil {
			t.Fatal("historical fixture requires its registered immutable payload")
		}
		if _, err := service.runDetailStore().ReadDetail(t.Context(), string(scope.Organization), *snapshot.Payload); err != nil {
			t.Fatal(err)
		}
		if snapshot.CheckpointPayload != nil {
			if _, err := service.runDetailStore().ReadDetail(t.Context(), string(scope.Organization), *snapshot.CheckpointPayload); err != nil {
				t.Fatal(err)
			}
		}
		if includePolicy {
			if _, err := nativeAttemptTestDatabase(t, local.nativeFixture).ExecContext(t.Context(), "UPDATE lease_policy_bindings SET selector_runner_id=NULL,selector_machine_id=NULL,selector_tags=NULL WHERE organization_id=$1 AND lease_id=$2", scope.Organization, actual.LeaseID); err != nil {
				t.Fatal(err)
			}
		}
		var pinned policystore.LeasePolicy
		if err := service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			pinned, err = r.LeasePolicy(t.Context(), actual.LeaseID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		recovered, err := service.readNativeAttemptSnapshotTenant(t.Context(), scope, string(local.issue.WorkItemID), id)
		if err != nil {
			t.Fatal(err)
		}
		run, err := service.readNativeAttemptRunTenant(t.Context(), recovered)
		if err != nil || !reflect.DeepEqual(run, actual) || !reflect.DeepEqual(recovered.Attempt, snapshot.Attempt) {
			t.Fatalf("historical import changed attempt payload or metadata: run=%+v error=%v", run, err)
		}
		if includePolicy {
			if err := service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
				current, err := r.LeasePolicy(t.Context(), actual.LeaseID)
				if err == nil && !reflect.DeepEqual(current, pinned) {
					t.Fatal("policy import changed the pinned policy")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, test := range []struct {
		name string
		want bool
	}{
		{"confirmed", true}, {"overwritten latest checkpoint", true},
		{"pending", false}, {"ambiguous", false}, {"missing checkpoint", false},
		{"foreign organization", false}, {"foreign project", false}, {"foreign work item", false},
		{"wrong recorder", false}, {"wrong run", false}, {"wrong lease", false}, {"wrong fence", false},
		{"wrong policy", false}, {"wrong pinned policy", false}, {"wrong attempt policy", false}, {"unrecorded sequence", false},
		{"wrong change", false}, {"wrong version", false}, {"wrong reference head", false}, {"wrong checkpoint head", false},
		{"latest malformed recorder", false}, {"legacy confirmed", true}, {"legacy wrong recorder", false},
		{"legacy overflow confirmed", true}, {"legacy overflow wrong recorder", false}, {"legacy overflow newer invalid", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			local := newNamedChangeFixture(t, service, "changes "+test.name)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPut, local.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: hubTestPolicy().ID, Policy: descriptor}), http.StatusOK)
			localRules := local.rules
			localRules.PolicyID = descriptor.ID
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPut, local.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "push-policy"}, ExpectedID: local.rules.ID, Policy: localRules}), http.StatusOK)
			localWorker := local.worker(t, "checkpoint-worker "+test.name)
			localLease := claimNativeAttempt(t, local.nativeFixture, localWorker, hubapi.NewID("machine"), hubapi.NewID("session"), local.issue.WorkItemID, descriptor.ID)
			localStart := nativeStartedEvent(localLease)
			localStart.Data.Identity.Role, localStart.Data.MachineID = "code", localLease.MachineID
			localStart.Data.Runtime = &tracker.NativeRuntimeObservation{LocalAttemptID: 42, Phase: "implementation", HeartbeatAt: now}
			localPath := local.base + "/work-items/" + string(local.issue.WorkItemID) + "/events"
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, localPath, localWorker, localStart), http.StatusOK)
			localInput := changeTestInput()
			localInput.PolicyID, localInput.RunID, localInput.AttemptID = descriptor.ID, localStart.Data.RunID, localStart.Data.AttemptID
			localBundle := changeTestSource(t, &localInput)
			localDiff := tracker.AttemptDiffRequest{Producer: tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: localStart.Data.AttemptID, LeaseID: localLease.ID, FencingToken: localLease.FencingToken}, Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 1}, BaseSHA: localInput.BaseSHA, HeadSHA: localInput.HeadSHA}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, local.base+"/attempts/"+localStart.Data.AttemptID+"/diff", localWorker, localDiff), http.StatusAccepted)
			response := performHubAPIRequest(t, service, http.MethodPost, local.path+"/versions", localWorker, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "push-source", LeaseID: localLease.ID, FencingToken: localLease.FencingToken}, ChangeVersionInput: localInput, SourceBundle: localBundle})
			requireNativeStatus(t, response, http.StatusOK)
			var localVersion tracker.ChangeVersion
			decodeHubResponse(t, response, &localVersion)
			localCheckpoint := localStart
			localCheckpoint.Type, localCheckpoint.IdempotencyKey, localCheckpoint.Data.Sequence = "run.checkpointed", "ordinary-checkpoint", 2
			localCheckpoint.Data.Handoff = nativeTestCheckpoint()
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, localPath, localWorker, localCheckpoint), http.StatusOK)
			localScope := nativeAttemptListingScope(t, local.nativeFixture)
			localRef := tracker.NativeChangeReference{ChangeID: localVersion.ChangeID, VersionID: localVersion.ID, HeadSHA: localVersion.HeadSHA}
			localBinding := tracker.NativePRPublication{ChangeID: localVersion.ChangeID, VersionID: localVersion.ID, Repository: localVersion.Repository, HeadSHA: localVersion.HeadSHA, PolicyID: localVersion.PolicyID, BaseRef: "main", Branch: "retained", SourceVersion: localRef, SourceAttemptID: localVersion.AttemptID}
			localBaseline := local.detail(t)
			original, originalPayload := sourceSnapshot(t, local, localVersion)
			content := eventstore.NewContent(service.runDetailStore())
			admin := nativeAttemptTestDatabase(t, local.nativeFixture)
			run := localCheckpoint.Data
			handoff := *run.Handoff
			reference := localRef
			handoff.Change, handoff.HeadSHA = &reference, localVersion.HeadSHA
			handoff.ExternalEffect, handoff.EffectState, handoff.EffectID = "git_push", "confirmed", localBinding.EffectID("git_push")
			run.Handoff = &handoff
			eventScope, eventItem := localScope, local.issue.WorkItemID
			var retained []eventstore.Event
			appendCheckpoint := func(unknown bool) {
				t.Helper()
				raw, err := json.Marshal(tracker.CollaborationEvent{ID: hubapi.NewID("event"), OrganizationID: eventScope.Organization, ProjectID: eventScope.Project, AggregateType: "issue", AggregateID: eventItem, Type: "run.checkpointed", SchemaVersion: 1, Actor: eventScope.Actor(), RecordedAt: now, Data: tracker.CollaborationData{Run: &run}})
				if err != nil {
					t.Fatal(err)
				}
				row, err := content.PrepareEvent(t.Context(), eventScope.Organization, raw)
				if err != nil {
					t.Fatal(err)
				}
				if unknown {
					row.PushKnown = false
				}
				if err := service.config.TenantRepository.WriteTenant(t.Context(), eventScope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
					var err error
					row, err = w.AppendEvent(t.Context(), row)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				retained = append(retained, row)
			}
			switch test.name {
			case "pending", "ambiguous":
				handoff.EffectState = test.name
			case "missing checkpoint":
				run.Handoff = nil
			case "foreign organization", "foreign project":
				organization := localScope.Organization
				if test.name == "foreign organization" {
					organization = "org_first_second"
				}
				foreign := newNativeFixture(t, service, organization, "foreign-checkpoint")
				eventScope = nativeAttemptListingScope(t, foreign)
				eventItem = foreign.create(t, "foreign checkpoint").WorkItemID
			case "foreign work item":
				eventItem = local.create(t, "foreign checkpoint").WorkItemID
			case "legacy overflow newer invalid", "latest malformed recorder":
				appendCheckpoint(strings.HasPrefix(test.name, "legacy "))
				fallthrough
			case "wrong recorder", "legacy wrong recorder", "legacy overflow wrong recorder":
				run.AttemptID = hubapi.NewID("attempt")
			case "wrong run":
				run.RunID = hubapi.NewID("run")
			case "wrong lease":
				run.LeaseID = "foreign-lease"
			case "wrong fence":
				run.FencingToken++
			case "wrong policy":
				run.PolicyID = hubTestPolicy().ID
			case "wrong pinned policy":
				if _, err := admin.ExecContext(t.Context(), "UPDATE lease_policy_bindings SET policy_id=$1 WHERE organization_id=$2 AND lease_id=$3", hubTestPolicy().ID, localScope.Organization, localLease.ID); err != nil {
					t.Fatal(err)
				}
			case "wrong attempt policy":
				snapshot, err := service.readNativeAttemptSnapshotTenant(t.Context(), localScope, string(local.issue.WorkItemID), localStart.Data.AttemptID)
				if err != nil {
					t.Fatal(err)
				}
				actual, err := service.readNativeAttemptRunTenant(t.Context(), snapshot)
				if err != nil {
					t.Fatal(err)
				}
				actual.PolicyID = hubTestPolicy().ID
				raw, err := json.Marshal(actual)
				if err != nil {
					t.Fatal(err)
				}
				ref, err := service.runDetailStore().StoreDetail(t.Context(), string(localScope.Organization), raw)
				if err != nil {
					t.Fatal(err)
				}
				if err := service.config.TenantRepository.WriteTenant(t.Context(), localScope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
					_, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: localScope.Organization, ID: hubapi.NewID("archive"), Reference: ref, CreatedAt: now})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), "UPDATE native_attempts SET payload_object_key=$1 WHERE organization_id=$2 AND id=$3", ref.Key, localScope.Organization, localStart.Data.AttemptID); err != nil {
					t.Fatal(err)
				}
			case "unrecorded sequence":
				run.Sequence++
			case "wrong change":
				reference.ChangeID = hubapi.NewID("change")
			case "wrong version":
				reference.VersionID = hubapi.NewID("version")
			case "wrong reference head":
				reference.HeadSHA = strings.Repeat("e", 40)
			case "wrong checkpoint head":
				handoff.HeadSHA = strings.Repeat("e", 40)
			}
			appendCheckpoint(strings.HasPrefix(test.name, "legacy "))
			if strings.HasPrefix(test.name, "legacy overflow") {
				run.Handoff = nil
				for range hubcontent.ClaimEvidenceLimit + 1 {
					appendCheckpoint(true)
				}
				if _, err := service.readBoundedChangePushes(t.Context(), localScope, string(local.issue.WorkItemID), []tracker.ChangeVersion{localVersion}); !errors.Is(err, corestore.ErrInvalidPage) {
					t.Fatalf("unknown overflow refusal = %v", err)
				}
			}
			if test.name == "overwritten latest checkpoint" {
				if _, err := admin.ExecContext(t.Context(), "UPDATE native_attempts SET checkpoint_object_key=NULL,checkpoint_preserves_source=NULL WHERE organization_id=$1 AND id=$2", localScope.Organization, localStart.Data.AttemptID); err != nil {
					t.Fatal(err)
				}
			}
			backfill := func() {
				t.Helper()
				total := 0
				for {
					count, err := service.config.TenantRepository.(*postgres.Repository).BackfillChangeRecoveryProjections(t.Context(), eventScope.Organization, content, 7)
					if err != nil || count < 0 || count > 7 {
						t.Fatalf("bounded checkpoint recovery=%d error=%v", count, err)
					}
					total += count
					if count == 0 {
						break
					}
					if total > len(retained)+corestore.MaxPageSize {
						t.Fatal("checkpoint recovery did not converge")
					}
				}
				var remaining int
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND type='run.checkpointed' AND NOT push_known", eventScope.Organization, eventScope.Project, eventItem).Scan(&remaining); err != nil || remaining != 0 {
					t.Fatalf("checkpoint projection incomplete: remaining=%d error=%v", remaining, err)
				}
			}
			backfill()
			for _, projection := range []string{"typed", "unconverted event", "unconverted attempt", "unconverted policy"} {
				t.Run(projection, func(t *testing.T) {
					if projection == "unconverted event" {
						for _, row := range retained {
							value, err := content.Event(t.Context(), row)
							if err != nil {
								t.Fatal(err)
							}
							value.ID, value.AggregateSequence = hubapi.NewID("event"), 0
							raw, err := json.Marshal(value)
							if err != nil {
								t.Fatal(err)
							}
							historical, err := content.PrepareEvent(t.Context(), eventScope.Organization, raw)
							if err != nil {
								t.Fatal(err)
							}
							historical.PushKnown = false
							if err := service.config.TenantRepository.WriteTenant(t.Context(), eventScope.Organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), historical); return err }); err != nil {
								t.Fatal(err)
							}
						}
						backfill()
					}
					if projection == "unconverted attempt" || projection == "unconverted policy" {
						verifyHistoricalAttempt(t, local, localStart.Data.AttemptID, projection == "unconverted policy")
					}
					proofs, err := service.readBoundedChangePushes(t.Context(), localScope, string(local.issue.WorkItemID), []tracker.ChangeVersion{localVersion})
					found := len(proofs) == 1
					if err != nil || found != test.want || len(proofs) > 1 {
						t.Fatalf("%s: proof=%+v want=%v error=%v", projection, proofs, test.want, err)
					}
					if found && (proofs[0].Change != localRef || proofs[0].RecordingAttemptID != localStart.Data.AttemptID || proofs[0].EffectID != localBinding.EffectID("git_push")) {
						t.Fatalf("%s: checkpoint attribution changed: %+v", projection, proofs[0])
					}
					if detail := local.detail(t); !reflect.DeepEqual(detail.Versions, localBaseline.Versions) || !reflect.DeepEqual(detail.Reviews, localBaseline.Reviews) || !reflect.DeepEqual(detail.Summary, localBaseline.Summary) || detail.External != nil {
						t.Fatalf("push proof mutated source or review evidence: detail=%+v", detail)
					}
					current, currentPayload := sourceSnapshot(t, local, localVersion)
					if !reflect.DeepEqual(current, original) || !reflect.DeepEqual(currentPayload, originalPayload) {
						t.Fatal("immutable source snapshot changed")
					}
				})
			}
			if test.name == "confirmed" {
				for _, payloadCase := range []string{"missing attempt payload", "mismatched attempt payload"} {
					t.Run(payloadCase, func(t *testing.T) {
						snapshot, err := service.readNativeAttemptSnapshotTenant(t.Context(), localScope, string(local.issue.WorkItemID), localStart.Data.AttemptID)
						if err != nil {
							t.Fatal(err)
						}
						if snapshot.Payload == nil {
							t.Fatal("payload refusal fixture requires its registered immutable payload")
						}
						originalRef := *snapshot.Payload
						payload, err := service.runDetailStore().ReadDetail(t.Context(), string(localScope.Organization), originalRef)
						if err != nil {
							t.Fatal(err)
						}
						switch payloadCase {
						case "missing attempt payload":
							t.Cleanup(func() {
								cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubapi.ReadTimeout)
								defer cancel()
								restored, err := details.StoreDetail(cleanup, string(localScope.Organization), payload)
								if err != nil || restored != originalRef {
									t.Errorf("restore missing attempt object: reference=%+v error=%v", restored, err)
								}
							})
							details.mu.Lock()
							delete(details.data, originalRef.Key)
							details.mu.Unlock()
						case "mismatched attempt payload":
							var mismatched tracker.NativeRunData
							if err := json.Unmarshal(payload, &mismatched); err != nil {
								t.Fatal(err)
							}
							mismatched.RunID = hubapi.NewID("run")
							raw, err := json.Marshal(mismatched)
							if err != nil {
								t.Fatal(err)
							}
							ref, err := service.runDetailStore().StoreDetail(t.Context(), string(localScope.Organization), raw)
							if err != nil {
								t.Fatal(err)
							}
							if err := service.config.TenantRepository.WriteTenant(t.Context(), localScope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
								_, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: localScope.Organization, ID: hubapi.NewID("archive"), Reference: ref, CreatedAt: now})
								return err
							}); err != nil {
								t.Fatal(err)
							}
							t.Cleanup(func() {
								cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubapi.ReadTimeout)
								defer cancel()
								if _, err := admin.ExecContext(cleanup, "UPDATE native_attempts SET payload_object_key=$1 WHERE organization_id=$2 AND id=$3", originalRef.Key, localScope.Organization, localStart.Data.AttemptID); err != nil {
									t.Errorf("restore attempt payload reference: %v", err)
								}
							})
							if _, err := admin.ExecContext(t.Context(), "UPDATE native_attempts SET payload_object_key=$1 WHERE organization_id=$2 AND id=$3", ref.Key, localScope.Organization, localStart.Data.AttemptID); err != nil {
								t.Fatal(err)
							}
						}
						proofs, err := service.readBoundedChangePushes(t.Context(), localScope, string(local.issue.WorkItemID), []tracker.ChangeVersion{localVersion})
						if err == nil || proofs != nil {
							t.Fatalf("unavailable required attempt evidence yielded a successful read: proofs=%+v error=%v", proofs, err)
						}
						switch payloadCase {
						case "missing attempt payload":
							if !errors.Is(err, artifact.ErrMissing) || !errors.Is(err, artifact.ErrIntegrity) {
								t.Fatalf("missing attempt payload error = %v", err)
							}
						case "mismatched attempt payload":
							var mismatch *hubrecords.NativeAttemptIdentityMismatch
							if !errors.Is(err, artifact.ErrIntegrity) || !errors.As(err, &mismatch) || mismatch.Field != "run_id" || mismatch.AttemptID != localStart.Data.AttemptID || mismatch.WorkItemID != local.issue.WorkItemID {
								t.Fatalf("mismatched attempt payload error = %v", err)
							}
						}
					})
				}
				proofs, err := service.readBoundedChangePushes(t.Context(), localScope, string(local.issue.WorkItemID), []tracker.ChangeVersion{localVersion})
				if err != nil || len(proofs) != 1 || proofs[0].Change != localRef || proofs[0].RecordingAttemptID != localStart.Data.AttemptID || proofs[0].EffectID != localBinding.EffectID("git_push") {
					t.Fatalf("restored attempt payload lost original attribution: proofs=%+v error=%v", proofs, err)
				}
			}
			executionState := func(t *testing.T) (leasestore.Lease, leasestore.Attempt) {
				t.Helper()
				var lease leasestore.Lease
				var attempt leasestore.Attempt
				if err := service.config.TenantRepository.ReadTenant(t.Context(), localScope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					if lease, err = r.Lease(t.Context(), string(localLease.ID)); err != nil {
						return err
					}
					attempt, err = r.LatestAttempt(t.Context(), lease.IssueID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return lease, attempt
			}
			heldLease, runningAttempt := executionState(t)
			release := performHubAPIRequest(t, service, http.MethodPost, local.base+"/leases/"+string(localLease.ID)+"/release", localWorker, tracker.NativeLeaseMutation{FencingToken: localLease.FencingToken, Reason: "released"})
			refusal, refused := map[string]string{
				"wrong run": "run_sequence_conflict", "unrecorded sequence": "run_sequence_conflict",
				"wrong lease": "stale_fencing_token", "wrong fence": "stale_fencing_token",
				"wrong policy": "policy_mismatch", "wrong pinned policy": "policy_mismatch",
			}[test.name]
			if !refused {
				requireNativeStatus(t, release, http.StatusNoContent)
				return
			}
			requireNativeCode(t, release, http.StatusConflict, refusal)
			lease, attempt := executionState(t)
			if lease.ReleasedAt != nil || lease.ReleaseReason != "" || !reflect.DeepEqual(lease, heldLease) {
				t.Fatalf("refused release changed the lease: before=%+v after=%+v", heldLease, lease)
			}
			if attempt.Status != "running" || !reflect.DeepEqual(attempt, runningAttempt) {
				t.Fatalf("refused release settled the attempt: before=%+v after=%+v", runningAttempt, attempt)
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
			foreignWorker := f.worker(t, "linked-push-worker")
			foreignLease := claimNativeAttempt(t, f.nativeFixture, foreignWorker, "linked-machine", "linked-session", foreignItem.WorkItemID, descriptor.ID)
			foreignStart := nativeStartedEvent(foreignLease)
			foreignStart.Data.Identity.Role, foreignStart.Data.MachineID = "code", foreignLease.MachineID
			foreignPath := f.base + "/work-items/" + string(foreignItem.WorkItemID) + "/events"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, foreignPath, foreignWorker, foreignStart), http.StatusOK)
			foreignCheckpoint := foreignStart
			foreignCheckpoint.IdempotencyKey, foreignCheckpoint.Type, foreignCheckpoint.Data.Sequence = "linked-confirmed-effect", "run.checkpointed", 2
			foreignHandoff := *checkpoint.Data.Handoff
			foreignCheckpoint.Data.Handoff = &foreignHandoff
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, foreignPath, foreignWorker, foreignCheckpoint), http.StatusOK)
			foreignDiff := diff
			foreignDiff.Producer = tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: foreignStart.Data.AttemptID, LeaseID: foreignLease.ID, FencingToken: foreignLease.FencingToken}
			foreignDiff.Generation.Seq = 3
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/attempts/"+foreignStart.Data.AttemptID+"/diff", foreignWorker, foreignDiff), http.StatusAccepted)
			credential, _, err := f.service.authenticateAPIToken(t.Context(), foreignWorker, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: foreignItem.WorkItemID, ChangeID: version.ChangeID, CreatedAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			foreignCheckpoint.IdempotencyKey, foreignCheckpoint.Data.Sequence = "linked-pr-receipt", 3
			foreignCheckpoint.Data.Handoff.Publication = &binding
			if err := validateNativeRunEvent(foreignCheckpoint); err != nil {
				t.Fatal(err)
			}
			foreignScope := scope
			foreignScope.Credential = credential
			_, err = service.appendNativeRunEventTenant(t.Context(), foreignScope, string(foreignItem.WorkItemID), foreignCheckpoint)
			var refusal *hubapi.NativeError
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
		after, afterPayload := sourceSnapshot(t, f, version)
		if !reflect.DeepEqual(after, snapshot) || !reflect.DeepEqual(afterPayload, snapshotPayload) {
			t.Fatal("early linkage rewrote immutable source")
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
		attempt, err := service.readNativeAttemptTenant(t.Context(), scope, string(f.issue.WorkItemID), start.Data.AttemptID)
		if err != nil || attempt.Status != "running" || attempt.Sequence != 7 {
			t.Fatalf("early publication changed attempt completion: status=%s sequence=%d error=%v", attempt.Status, attempt.Sequence, err)
		}
		if attempt.Finalization != nil {
			t.Fatalf("early publication finalized the attempt: finalization=%+v", attempt.Finalization)
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
		if err := service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error { return w.RevokeAccessCredential(t.Context(), credential.ID, now) }); err != nil {
			t.Fatal(err)
		}
		checkpoint.IdempotencyKey, checkpoint.Data.Sequence = "revoked-pr-receipt", 8
		if response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, checkpoint); response.Code != http.StatusUnauthorized {
			t.Fatalf("revoked owner recorded publication: %d", response.Code)
		}
	})
}

func TestChangeApprovalPreservesProtectedMerge(t *testing.T) {
	t.Parallel()
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("historical_pending_%t", pending), func(t *testing.T) {
			backend := &recordingOutboxBackend{verifyFailures: []error{Permanent(errors.New("required GitHub review and fresh checks unavailable"))}}
			service := newPostgresNativeService(t, Config{})
			service.config.OutboxBackend = backend
			f := newChangeFixture(t, service)
			if err := service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				project, err := w.ProjectRecord(t.Context(), f.project.ID)
				if err != nil {
					return err
				}
				project.Settings.Repository, project.Settings.CheckoutRepository, project.Settings.RepositoryEnabled = "example/repo", "example/repo", true
				_, err = w.UpdateProjectRecord(t.Context(), project)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			input := changeTestInput()
			input.External = &tracker.ChangeExternalReference{Provider: "github", ID: "1", URL: "https://github.com/example/repo/pull/1"}
			var version tracker.ChangeVersion
			if !pending {
				response := performHubAPIRequest(t, service, http.MethodPost, f.path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "external-version"}, ChangeVersionInput: input})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &version)
			} else {
				now := service.config.Clock().UTC().Truncate(time.Microsecond)
				version = tracker.ChangeVersion{ChangeVersionInput: input, ID: "version_historical", ChangeID: "change_historical", Number: 1, Policy: hubTestPolicy(), ReviewPolicy: f.rules, Actor: tracker.Actor{Kind: "human", PrincipalID: "example-owner"}, CreatedAt: now}
				for index, check := range f.rules.RequiredChecks {
					version.Checks = append(version.Checks, tracker.ChangeCheckExpectation{ChangeCheckSpec: check, CheckRunID: fmt.Sprintf("check_historical_%d", index)})
				}
				f.change = tracker.ChangeRequest{ID: version.ChangeID, OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: f.issue.WorkItemID, LinkedIssues: []tracker.NativeWorkItemID{f.issue.WorkItemID}, Title: "Imported external change", CurrentVersion: version.ID, Revision: 2, CreatedAt: now, UpdatedAt: now}
				policyRaw, err := json.Marshal(hubTestPolicy())
				if err != nil {
					t.Fatal(err)
				}
				fixture := coreChangeNeutralFixture(t, f.project.OrganizationID, f.project.ID, now, hubTestPolicy(), policyRaw)
				for table := range fixture.Records {
					fixture.Records[table] = nil
				}
				changeRaw, err := json.Marshal(f.change)
				if err != nil {
					t.Fatal(err)
				}
				versionRaw, err := json.Marshal(version)
				if err != nil {
					t.Fatal(err)
				}
				fixture.Add("policy_revisions", map[string]any{"scope": string(f.project.OrganizationID) + "/" + string(f.project.ID), "policy_id": version.PolicyID, "metadata_json": string(policyRaw)})
				fixture.Add("change_requests", map[string]any{"id": f.change.ID, "organization_id": f.project.OrganizationID, "project_id": f.project.ID, "work_item_id": f.issue.WorkItemID, "record_json": string(changeRaw)})
				fixture.Add("change_issue_links", map[string]any{"change_id": f.change.ID, "organization_id": f.project.OrganizationID, "project_id": f.project.ID, "work_item_id": f.issue.WorkItemID})
				fixture.Add("change_versions", map[string]any{"id": version.ID, "change_id": f.change.ID, "number": 1, "record_json": string(versionRaw), "policy_revision_id": version.PolicyID, "payload_object_key": "", "repository": version.Repository, "created_at": hubapi.FormatHubTime(now), "head_sha": nil, "external_id": nil, "external_url": nil})
				source := coreimport.CoreChangeImportSource{BundlePath: fixture.Write(), OrganizationID: f.project.OrganizationID, SourceObjects: service.runDetailStore(), Objects: service.runDetailStore()}
				for range 2 {
					if count, err := coreimport.ImportCoreChanges(t.Context(), service.database.postgres, source); err != nil || count != 1 {
						t.Fatalf("pending historical version import = %d, error = %v", count, err)
					}
				}
				f.path = f.base + "/work-items/" + string(f.issue.WorkItemID) + "/changes/" + f.change.ID
			}
			if err := service.refreshBoardRecord(t.Context(), f.project.OrganizationID, viewstore.BoardCandidate{ProjectID: f.project.ID, WorkItemID: f.issue.WorkItemID, Required: true}, service.config.Clock()); err != nil {
				t.Fatal(err)
			}
			projected := readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, "?include=change")
			if projected.Change == nil || projected.Change.Number != 1 || projected.Change.URL != "https://github.com/example/repo/pull/1" {
				t.Fatalf("pending=%v external change projection=%+v", pending, projected.Change)
			}
			key := projectstore.GitHubPullRequestRefreshKey{ProjectID: f.project.ID, ChangeID: version.ChangeID, Repository: "example/repo", Number: 1}
			observe := func(head string) error {
				t.Helper()
				at := service.config.Clock().UTC()
				if err := service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					return w.(projectstore.GitHubPullRequestRefreshWriter).RequestGitHubPullRequests(t.Context(), []projectstore.GitHubPullRequestRefresh{{GitHubPullRequestRefreshKey: key, WorkItemID: f.issue.WorkItemID, RequestedVersion: version.ID, RequestedHeadSHA: version.HeadSHA, RequestedAt: at}})
				}); err != nil {
					t.Fatal(err)
				}
				provider := &scriptedReconcileBackend{steps: []reconcileStep{{snapshot: ReconcileSnapshot{Repository: RepositorySource{NodeID: "R_example", Owner: "example", Name: "repo", UpdatedAt: at}, PullRequests: []PullRequestSource{{Number: 1, HeadSHA: head, UpdatedAt: at, State: "open"}}, PullRequestDetails: []PullRequestDetailSource{{Number: 1, HeadSHA: head, MergeableState: new("blocked"), Checks: &tracker.CheckSummary{Status: "completed", Conclusion: "failure", Total: 1, Failed: 1}, Reviews: &tracker.ReviewSummary{State: "known", Decision: "changes_requested", ChangesRequested: 1}}}}}}}
				service.config.ReconcileBackend = provider
				refreshErr := service.maintainGitHubPullRequestRefresh(t.Context())
				requests := provider.Requests()
				if len(requests) != 1 || len(requests[0].Hydrations) != 1 || requests[0].Hydrations[0].HeadSHA != version.HeadSHA || requests[0].Hydrations[0].GitHubNumber != 1 {
					t.Fatalf("refresh request lost immutable version authority: %+v", requests)
				}
				return refreshErr
			}
			if err := observe(version.HeadSHA); err != nil {
				t.Fatal(err)
			}
			path := f.path + "/versions/" + version.ID
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "native-approval"}, Decision: "approved"}), http.StatusOK)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path+"/checks", f.token, changeTestResult(version)), http.StatusOK)
			detail := f.detail(t)
			if detail.Summary.NativeReview != "approved" || detail.Summary.ExternalReview != "snapshot: changes_requested" || detail.External == nil || detail.External.Merge.Ready {
				t.Fatalf("native and external review authority = %#v", detail)
			}
			raw, err := json.Marshal(MergePullRequestDesired{PullRequestNumber: 1, HeadSHA: version.HeadSHA, MergeMethod: "squash"})
			if err != nil {
				t.Fatal(err)
			}
			payload, err := eventstore.NewContent(service.runDetailStore()).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: f.project.OrganizationID}, raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				return w.(projectstore.GitHubOutboxWriter).EnqueueGitHubOutbound(t.Context(), projectstore.GitHubOutbound{ProjectID: f.project.ID, WorkItemID: f.issue.WorkItemID, Key: "protected-merge", Repository: "example/repo", IssueNumber: int64(f.issue.Number), Kind: string(MutationMergePullRequest), CreatedAt: service.config.Clock(), Payload: payload})
			}); err != nil {
				t.Fatal(err)
			}
			processed, err := service.ProcessOutbox(t.Context())
			if processed || err == nil || backend.verifyCalls != 1 || backend.executeCalls != 0 || backend.effects != 0 {
				t.Fatalf("protected merge bypassed: processed=%v error=%v backend=%#v", processed, err, backend)
			}
			if err := observe(strings.Repeat("c", 40)); err == nil || !strings.Contains(err.Error(), "pull request refresh identity or head changed") {
				t.Fatalf("refresh for the approved head applied a moved head: %v", err)
			}
			if moved := f.detail(t); moved.Summary.NativeReview != "approved" || moved.External != nil && moved.External.Merge.Ready {
				t.Fatalf("moved head refusal changed the approved head's review state: %#v", moved)
			}
			if err := service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.(projectstore.GitHubPullRequestRefreshReader).GitHubPullRequestRefresh(t.Context(), key)
				if err != nil {
					return err
				}
				if row.Pending || row.RequestedHeadSHA != version.HeadSHA || row.Observation == nil || row.Observation.HeadSHA != version.HeadSHA {
					return fmt.Errorf("moved head refusal recorded head B for head A: %+v", row)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type landingCapacityFixture struct {
	nativeFixture
	descriptor  policy.Descriptor
	requirement providercapacity.Requirement
}

func setChangeFixtureStates(t *testing.T, f nativeFixture, states []tracker.NativeState) {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		record.States = states
		_, err = w.UpdateProjectRecord(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func newLandingCapacityFixture(t *testing.T, service *Service, name string, github bool) landingCapacityFixture {
	t.Helper()
	f := newNativeFixture(t, service, "", name)
	setChangeFixtureStates(t, f, HostedProjectStates())
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

func (f landingCapacityFixture) claim(t *testing.T, r runnerFixture, issue tracker.NativeIssue, session string, capacity int, states ...string) *httptest.ResponseRecorder {
	t.Helper()
	claim := tracker.NativeClaim{LandingCapacity: capacity, WorkflowStates: states, PolicyID: f.descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: session, TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
	if f.requirement.Backend != "" {
		claim.ProviderCandidates = []tracker.NativeCapacityCandidate{{WorkItemID: issue.WorkItemID, Revision: issue.Revision, Requirement: f.requirement}}
	}
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
}

func TestGitHubLandingCapacityAcrossRunners(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                                                                                                                                string
		admission                                                                                                                           string
		ownerCapacity, contenderCapacity, renewalCapacity, replayCapacity, expiredRenewalCapacity                                           int
		boundary                                                                                                                            string
		nativeOnly, otherProject, laneChanged, expired, concurrent, fallback, room, renewFree, renewRace, direct, directRace, renewHostBusy bool
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
		{name: "expired renewal rechecks runner occupancy", ownerCapacity: 3, contenderCapacity: 3, expired: true, renewHostBusy: true},
		{name: "expired changed-lane owner cannot renew an occupied slot", ownerCapacity: 1, contenderCapacity: 1, expired: true, laneChanged: true},
		{name: "negative capacity is invalid", ownerCapacity: -1},
		{name: "separate project proceeds", ownerCapacity: 1, contenderCapacity: 1, otherProject: true, admission: "ready"},
		{name: "native-only landing requires provider selection", ownerCapacity: 1, contenderCapacity: 1, nativeOnly: true, admission: "unknown"},
		{name: "concurrent claims acquire one slot", ownerCapacity: 1, contenderCapacity: 1, concurrent: true},
		{name: "expired renewal races a new claim", ownerCapacity: 1, contenderCapacity: 1, concurrent: true, renewRace: true},
		{name: "busy landing permits other ready work", ownerCapacity: 1, contenderCapacity: 1, fallback: true},
		{name: "direct landing shares project admission", ownerCapacity: 1, contenderCapacity: 1, direct: true},
		{name: "concurrent direct transitions acquire one slot", ownerCapacity: 1, contenderCapacity: 1, concurrent: true, directRace: true},
		{name: "integral current second retains fractional future expiry", ownerCapacity: 1, contenderCapacity: 1, boundary: "integral"},
		{name: "fractional current second retains microsecond future expiry", ownerCapacity: 1, contenderCapacity: 1, boundary: "fractional"},
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
				service = newClockedPostgresNativeService(t, Config{Clock: func() time.Time { return now }})
			}
			f := newLandingCapacityFixture(t, service, "landing-capacity", !test.nativeOnly)
			first := f.ready(t, "first change")
			secondFixture := f
			if test.otherProject {
				secondFixture = newLandingCapacityFixture(t, f.service, "independent-landings", true)
			}
			second := secondFixture.ready(t, "second change")
			if test.nativeOnly {
				second = updateNativeWorkPageFixtureIssue(t, secondFixture.nativeFixture, second.WorkItemID, func(issue *tracker.NativeIssue) {
					issue.Labels = append(issue.Labels, "requires-human-review")
				})
			}
			if test.direct || test.directRace {
				second = updateNativeWorkPageFixtureIssue(t, f.nativeFixture, second.WorkItemID, func(issue *tracker.NativeIssue) { issue.State = "In Progress" })
			}
			if test.directRace {
				first = updateNativeWorkPageFixtureIssue(t, f.nativeFixture, first.WorkItemID, func(issue *tracker.NativeIssue) { issue.State = "In Progress" })
			}
			owner := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			owner.enroll(t)
			reportRunnerCapacity(t, f.service, f.base, owner.redemption.Credential, owner.binding.MachineID, owner.redemption.DisplayName, owner.redemption.BackendIsolation, owner.redemption.Capacity)
			waiter := prepareRunner(t, secondFixture.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Events, runnerauth.Heartbeat)
			waiter.enroll(t)
			reportRunnerCapacity(t, secondFixture.service, secondFixture.base, waiter.redemption.Credential, waiter.binding.MachineID, waiter.redemption.DisplayName, waiter.redemption.BackendIsolation, waiter.redemption.Capacity)
			if test.nativeOnly {
				publishCapacity(t, secondFixture.nativeFixture, waiter, capacityReport(f.service.config.Clock()))
			}
			ownerSession, waiterSession := "first-landing", "waiting-landing"
			if test.ownerCapacity < 0 {
				requireNativeStatus(t, f.claim(t, owner, first, ownerSession, test.ownerCapacity), http.StatusUnprocessableEntity)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims/preview", owner.redemption.Credential, tracker.NativeCapacityPreview{NativeClaim: tracker.NativeClaim{LandingCapacity: test.ownerCapacity, PolicyID: f.descriptor.ID, MachineID: owner.binding.MachineID}}), http.StatusUnprocessableEntity)
				return
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
					if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE leases SET acquired_at=LEAST(acquired_at,$1::timestamptz-interval '1 minute'),renewed_at=LEAST(renewed_at,$1::timestamptz-interval '1 minute'),expires_at=$1 WHERE organization_id=$2 AND lease_id=$3", f.service.config.Clock().Add(-time.Second), f.project.OrganizationID, lease.ID); err != nil {
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
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.Lease(t.Context(), string(lease.ID))
				pinned = int(row.LandingCapacity)
				return err
			}); err != nil {
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
				var versionRow eventstore.Version
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), secondFixture.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					row, err := r.LatestChange(t.Context(), secondFixture.project.ID, second.WorkItemID)
					if err != nil {
						return err
					}
					if row.CurrentVersionID == nil {
						return errors.New("landing fixture has no current version")
					}
					change, version = row.ID, *row.CurrentVersionID
					versionRow, err = r.Version(t.Context(), change, version)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				published, err := eventstore.NewContent(f.service.runDetailStore()).Version(t.Context(), versionRow)
				if err != nil {
					t.Fatal(err)
				}
				event := nativeStartedEvent(direct)
				path := f.base + "/work-items/" + string(second.WorkItemID)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", waiter.redemption.Credential, event), http.StatusOK)
				event.Type, event.IdempotencyKey, event.Data.Sequence = "run.observed", "landing-wait", 2
				event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: f.service.config.Clock(), Landing: &tracker.NativeLandingReceipt{ChangeID: change, VersionID: version, HeadSHA: published.HeadSHA, Waiting: true}}
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
				response = performHubAPIRequest(t, f.service, http.MethodGet, path+"?view=recovery", f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var recovery tracker.NativeRecovery
				decodeHubResponse(t, response, &recovery)
				projected := recovery.Issue
				if projected.ListChange == nil || !projected.ListChange.LandingPending || projected.ListChange.HeadSHA != published.HeadSHA {
					t.Fatalf("waiting direct landing lost source intent: %#v", projected.ListChange)
				}
				requireNativeStatus(t, admit(test.contenderCapacity), http.StatusOK)
				requireNativeStatus(t, admit(test.contenderCapacity+1), http.StatusOK)
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					row, err := r.Lease(t.Context(), string(direct.ID))
					pinned = int(row.LandingCapacity)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if pinned != test.contenderCapacity {
					t.Fatalf("direct admission replay changed ceiling to%d, want%d", pinned, test.contenderCapacity)
				}
				third := f.ready(t, "third change")
				requireNativeStatus(t, f.claim(t, owner, third, "next-landing", test.ownerCapacity), http.StatusConflict)
				updateNativeWorkPageFixtureIssue(t, f.nativeFixture, second.WorkItemID, func(*tracker.NativeIssue) {})
				response = performHubAPIRequest(t, f.service, http.MethodGet, path+"?view=recovery", f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				recovery = tracker.NativeRecovery{}
				decodeHubResponse(t, response, &recovery)
				projected = recovery.Issue
				if projected.ListChange != nil && projected.ListChange.LandingPending {
					t.Fatal("operator revision change retained stale direct-landing intent")
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(direct.ID)+"/release", waiter.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: direct.FencingToken, Reason: "completed"}), http.StatusNoContent)
				requireNativeStatus(t, f.claim(t, owner, third, "next-landing", test.ownerCapacity), http.StatusOK)
				return
			}
			if test.laneChanged {
				first = updateNativeWorkPageFixtureIssue(t, f.nativeFixture, first.WorkItemID, func(issue *tracker.NativeIssue) { issue.State = "In Progress" })
			}
			if test.expired {
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE leases SET acquired_at=LEAST(acquired_at,$1::timestamptz-interval '1 minute'),renewed_at=LEAST(renewed_at,$1::timestamptz-interval '1 minute'),expires_at=$1 WHERE organization_id=$2 AND lease_id=$3", f.service.config.Clock().Add(-time.Second), f.project.OrganizationID, lease.ID); err != nil {
					t.Fatal(err)
				}
			}
			if test.boundary != "" {
				expiry := f.service.config.Clock()
				switch test.boundary {
				case "integral":
					expiry = expiry.Add(100 * time.Millisecond)
				case "fractional":
					expiry = expiry.Add(time.Microsecond)
				}
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE leases SET acquired_at=LEAST(acquired_at,$1::timestamptz-interval '1 minute'),renewed_at=LEAST(renewed_at,$1::timestamptz-interval '1 minute'),expires_at=$1 WHERE organization_id=$2 AND lease_id=$3", expiry, f.project.OrganizationID, lease.ID); err != nil {
					t.Fatal(err)
				}
			}
			if test.renewHostBusy {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, owner.identityPath()+"/routing", testHubAdminToken, map[string]any{"expected_revision": 1, "display_name": "Example runner", "state": "active", "capacity_limit": 1, "project_ids": []tracker.ProjectID{f.project.ID}, "idempotency_key": "renewal-occupancy"}), http.StatusOK)
				busy := f.ready(t, "replacement slot")
				requireNativeStatus(t, f.claim(t, owner, busy, "replacement-slot", test.ownerCapacity), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", owner.redemption.Credential, tracker.NativeLeaseMutation{LandingCapacity: test.ownerCapacity, FencingToken: lease.FencingToken, TTLSeconds: 90}), http.StatusConflict)
				return
			}
			if test.renewFree {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", owner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), http.StatusOK)
			}
			want := http.StatusConflict
			if test.nativeOnly || test.otherProject || test.expired && !test.renewFree || test.room {
				want = http.StatusOK
			}
			claimStates := []string{"Todo", "In Progress"}
			preview := performHubAPIRequest(t, f.service, http.MethodPost, secondFixture.base+"/claims/preview", waiter.redemption.Credential, tracker.NativeCapacityPreview{NativeClaim: tracker.NativeClaim{LandingCapacity: test.contenderCapacity, PolicyID: secondFixture.descriptor.ID, MachineID: waiter.binding.MachineID, WorkflowStates: claimStates}})
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
			if test.admission != "" {
				readEvidence := func(selectors tracker.NativeAdmissionContext) tracker.NativeRuntimeEvidence {
					t.Helper()
					selectors.ObservedAt, selectors.PolicyID, selectors.WorkflowStates = f.service.config.Clock(), secondFixture.descriptor.ID, claimStates
					raw, err := json.Marshal(selectors)
					if err != nil {
						t.Fatal(err)
					}
					path := secondFixture.base + "/work-items/" + string(second.WorkItemID) + "/runtime?admission=" + url.QueryEscape(string(raw))
					response := performHubAPIRequest(t, f.service, http.MethodGet, path, waiter.redemption.Credential, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var evidence tracker.NativeRuntimeEvidence
					decodeHubResponse(t, response, &evidence)
					return evidence
				}
				evidence := readEvidence(tracker.NativeAdmissionContext{})
				found := false
				for _, admission := range evidence.Admission {
					if admission.RunnerID != waiter.binding.RunnerID {
						continue
					}
					found = true
					if admission.Outcome != test.admission || slices.Contains(admission.Unavailable, "project_landing_capacity") != (test.admission == "unknown" && !test.nativeOnly) || test.nativeOnly && !slices.Contains(admission.Unavailable, "provider_candidate_requirement") {
						t.Fatalf("runtime admission invents incoming capacity: %#v", admission)
					}
				}
				if !found {
					t.Fatal("runtime lost the contender's admission evidence")
				}
				if evidence.Scheduling.Outcome != test.admission {
					t.Fatalf("runtime scheduling claims an unproved ceiling: %#v", evidence.Scheduling)
				}
				if test.nativeOnly {
					for _, selectors := range []tracker.NativeAdmissionContext{
						{Authors: []string{"other-author"}},
						{Assignees: []string{"other-assignee"}},
						{LabelInclude: []string{"absent-label"}},
						{LabelExclude: []string{"requires-human-review"}},
					} {
						evidence := readEvidence(selectors)
						if !slices.ContainsFunc(evidence.Admission, func(entry tracker.NativeRuntimeAdmission) bool {
							return entry.RunnerID == waiter.binding.RunnerID && entry.Outcome == "skipped" && entry.Reason == "Current claim selectors do not select this item"
						}) {
							t.Fatalf("historical landing bypassed selectors %+v: %+v", selectors, evidence.Admission)
						}
					}
				}
			}
			if test.nativeOnly {
				refused := secondFixture.claim(t, waiter, second, waiterSession, test.contenderCapacity, claimStates...)
				requireNativeStatus(t, refused, http.StatusConflict)
				secondFixture.requirement = providercapacity.Requirement{Role: "merge", Backend: "codex", Model: "test-model"}
			}
			claimed := secondFixture.claim(t, waiter, second, waiterSession, test.contenderCapacity, claimStates...)
			requireNativeStatus(t, claimed, want)
			if test.nativeOnly {
				var lease tracker.NativeLease
				decodeHubResponse(t, claimed, &lease)
				if lease.ProviderReservation == nil || lease.ProviderReservation.Requirement != secondFixture.requirement {
					t.Fatalf("landing did not reserve the selected provider: %+v", lease.ProviderReservation)
				}
			}
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
	f := newChangeFixture(t, newClockedPostgresNativeService(t, Config{Clock: func() time.Time { return now }}))
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
	updatedRules := f.rules
	updatedRules.PolicyID = updated.ID
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "updated-rules"}, ExpectedID: f.rules.ID, Policy: updatedRules}), http.StatusOK)
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
	stored := f.detail(t).Change.Landed
	if stored == nil || stored.VersionID != first.ID || stored.Quality == nil || stored.Quality.IssueBody != landed.Landed.Quality.IssueBody {
		t.Fatalf("persistent landing context = %+v", stored)
	}

	if err := f.service.refreshBoardRecord(t.Context(), f.project.OrganizationID, viewstore.BoardCandidate{ProjectID: f.project.ID, WorkItemID: f.issue.WorkItemID, Required: true}, f.service.config.Clock()); err != nil {
		t.Fatal(err)
	}
	projected := readWorkItem(t, f.nativeFixture, f.issue.WorkItemID, "?include=change")
	if projected.ListChange == nil || projected.ListChange.Branch != land.BaseRef || projected.ListChange.HeadSHA != land.MergeSHA {
		t.Fatalf("list change = %#v", projected.ListChange)
	}

	foreign := newNativeFixture(t, f.service, f.project.OrganizationID, "Foreign landing query scope")
	foreignItem := foreign.create(t, "Foreign item")
	scopeCredential := analyticsTestScope(t, f.nativeFixture).Credential
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: f.project.OrganizationID, TokenID: scopeCredential.ID, ProjectID: foreign.project.ID})
	}); err != nil {
		t.Fatal(err)
	}
	foreignScope := hubscope.NativeScope{Organization: foreign.project.OrganizationID, Project: foreign.project.ID, Credential: scopeCredential}
	seedAnalyticsTestChange(t, f.service, foreignScope, foreignItem.WorkItemID, "foreign-landed", now, &tracker.ChangeLanding{MergeSHA: strings.Repeat("f", 40), BaseRef: "develop", LandedAt: now})
	for _, test := range []struct {
		name     string
		scope    hubscope.NativeScope
		from, to time.Time
		want     int
	}{
		{"landed version", hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID}, now.Add(-time.Hour), now.Add(time.Hour), 1},
		{"exclusive end", hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID}, now.Add(-time.Hour), now, 0},
		{"outside window", hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID}, now.Add(time.Second), now.Add(time.Hour), 0},
		{"foreign project", hubscope.NativeScope{Organization: f.project.OrganizationID, Project: foreign.project.ID}, now.Add(-time.Hour), now.Add(time.Hour), 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			scope := test.scope
			scope.Credential = scopeCredential
			got, err := f.service.readNativeAnalyticsTenant(f.service.runDetailContext(t.Context()), scope, operatortool.AnalyticsRequest{Limit: 1}, operatortool.AnalyticsWindow{From: test.from, To: test.to, Bucket: time.Hour})

			if err != nil {
				t.Fatal(err)
			}
			if got.CostPerOutcome.Shipped != test.want || len(got.Landings.Items) != test.want {
				t.Fatalf("shipping evidence %#v", got)
			}
			if test.name == "foreign project" {
				if len(got.Landings.Items) != 1 || got.Landings.Items[0].ChangeID != "foreign-landed" {
					t.Fatalf("foreign query leaked main landing=%+v", got.Landings)
				}
				return
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
			setChangeFixtureStates(t, f, test.states)
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
				publishCapacity(t, f, r)
				if test.provider {
					publishCapacity(t, f, r, capacityReport(f.service.config.Clock()))
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
					var statements, writes atomic.Int64
					writeSQL := regexp.MustCompile(`(?im)^\s*(INSERT|UPDATE|DELETE|MERGE|TRUNCATE)\b`)
					traced := postgres.WithTransactionTiming(t.Context(), nil, func(_ time.Time, statement string) {
						statements.Add(1)
						if writeSQL.MatchString(statement) {
							writes.Add(1)
						}
					})
					for range 16 {
						request := newHubAPIRequest(t, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim).WithContext(traced)
						response := httptest.NewRecorder()
						f.service.Handler().ServeHTTP(response, request)
						requireNativeStatus(t, response, http.StatusConflict)
					}
					if statements.Load() == 0 || writes.Load() != 0 {
						t.Fatalf("unchanged decisions executed %d statements and %d writes", statements.Load(), writes.Load())
					}
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), http.StatusConflict)
				assertUnchangedDecision()
				decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, runtimePath, f.token, nil), &evidence)
				if evidence.Attempt != nil || evidence.LatestDecision == nil || evidence.LatestDecision.Actor.Kind != "runner" || evidence.LatestDecision.Data.Decision.Source != "native_runner_routing" || evidence.LatestDecision.Data.Decision.Outcome != "skipped" || evidence.LatestDecision.Data.Decision.RunnerID != r.binding.RunnerID || evidence.LatestDecision.Data.Decision.WorkItemRevision != issue.Revision {
					t.Fatalf("recorded routing refusal=%#v", evidence)
				}
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE machines SET capacity=0 WHERE organization_id=$1 AND id=$2", f.project.OrganizationID, r.binding.MachineID); err != nil {
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
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(issue.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "revision-change"}, ExpectedRevision: issue.Revision, Title: new("Land me again")}), http.StatusOK)
				if test.provider {
					claim.ProviderCandidates[0].Revision = issue.Revision + 1
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), http.StatusConflict)
				assertUnchangedDecision()
				decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, runtimePath, f.token, nil), &evidence)
				if evidence.LatestDecision.ID == previousDecision || evidence.LatestDecision.Data.Decision.WorkItemRevision != issue.Revision+1 {
					t.Fatalf("changed revision lost decision=%#v", evidence.LatestDecision)
				}
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE machines SET capacity=2 WHERE organization_id=$1 AND id=$2", f.project.OrganizationID, r.binding.MachineID); err != nil {
					t.Fatal(err)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, map[string]any{"expected_revision": 2, "display_name": "Landing runner", "state": "active", "capacity_limit": 2, "project_ids": []tracker.ProjectID{f.project.ID}}), http.StatusOK)
				var claimed, replayed tracker.NativeLease
				for _, lease := range []*tracker.NativeLease{&claimed, &replayed} {
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
					requireNativeStatus(t, response, http.StatusOK)
					decodeHubResponse(t, response, lease)
				}
				var decisions []eventstore.Event
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
					var err error
					decisions, err = reader.Events(t.Context(), f.project.ID, issue.WorkItemID, 0, 100)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if len(decisions) == 100 {
					t.Fatal("decision fixture history exceeds complete read bound")
				}
				var evaluatedClaims int
				for _, row := range decisions {
					if row.Type != "scheduler.decision" {
						continue
					}
					event, err := eventstore.NewContent(f.service.runDetailStoreFor(t.Context())).Event(t.Context(), row)
					if err != nil {
						t.Fatal(err)
					}
					if decision := event.Data.Decision; decision != nil && decision.Source == "native_claim" && decision.Outcome == "claimed" && decision.RunnerID == r.binding.RunnerID && decision.WorkItemRevision == issue.Revision+1 && event.Data.Change != nil && *event.Data.Change == (tracker.NativeChangeReference{ChangeID: change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}) {
						evaluatedClaims++
					}
				}
				if evaluatedClaims != 1 || claimed.ID == "" || replayed.ID != claimed.ID || claimed.WorkItemID != issue.WorkItemID {
					t.Fatalf("claimed landing evaluation of the reviewed Change: count=%d, want 1; claim=%+v replay=%+v", evaluatedClaims, claimed, replayed)
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
		name                    string
		require                 bool
		order                   []string
		expireDuringPreparation bool
	}{
		{name: "check after approval", require: true, order: []string{"approve", "check"}},
		{name: "approval after check", require: true, order: []string{"check", "approve"}},
		{name: "check expires before approval commits", require: true, order: []string{"check", "approve"}, expireDuringPreparation: true},
		{name: "no review required", require: false, order: []string{"check"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var service *Service
			var clock atomic.Int64
			if test.expireDuringPreparation {
				clock.Store(time.Now().UTC().UnixNano())
				service = newClockedPostgresNativeService(t, Config{Clock: func() time.Time { return time.Unix(0, clock.Load()).UTC() }})
			}
			f := newNativeFixture(t, service, "", "promote")
			setChangeFixtureStates(t, f, HostedProjectStates())
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
			if err != nil {
				t.Fatalf("operator credential status=%d error=%v", status, err)
			}
			principal := credential.ID
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
					request := newHubAPIRequest(t, http.MethodPost, path+"/versions/"+version.ID+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "approve"}, Decision: "approved"})
					if test.expireDuringPreparation {
						advanced := false
						ctx := postgres.WithTransactionTiming(t.Context(), nil, func(_ time.Time, query string) {
							if !advanced && strings.Contains(query, "name: RecordCurrentChangeReviewSequences") {
								advanced = true
								clock.Add(int64(time.Hour))
							}
						})
						request = request.WithContext(ctx)
					}
					response := httptest.NewRecorder()
					f.service.Handler().ServeHTTP(response, request)
					requireNativeStatus(t, response, http.StatusOK)
				case "check":
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/checks", f.token, changeTestResult(version)), http.StatusOK)
				}
				want := "Human Review"
				if i == len(test.order)-1 && !test.expireDuringPreparation {
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
			setChangeFixtureStates(t, f, HostedProjectStates())
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
			setChangeFixtureStates(t, f, HostedProjectStates())
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
			runner := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Events, runnerauth.Collaborate, runnerauth.Heartbeat)
			runner.enroll(t)
			reportRunnerCapacity(t, f.service, f.base, runner.redemption.Credential, runner.binding.MachineID, runner.redemption.DisplayName, runner.redemption.BackendIsolation, runner.redemption.Capacity)
			claimNumber := 0
			ready := func() bool {
				t.Helper()
				claimNumber++
				response := f.claim(t, runner, issue, fmt.Sprintf("readiness-%d", claimNumber), 1)
				requireNativeStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				if lease.ID == "" || lease.WorkItemID != issue.WorkItemID {
					t.Fatalf("landing claim = %+v", lease)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", runner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
				return lease.ID != "" && lease.WorkItemID == issue.WorkItemID
			}
			if !ready() {
				t.Fatal("reviewed version was not ready before any refusal")
			}
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/workflow"
			move := func(state, detail string) {
				t.Helper()
				current := readWorkItem(t, f.nativeFixture, issue.WorkItemID, "")
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
				current := readWorkItem(t, f.nativeFixture, issue.WorkItemID, "")
				response := f.claim(t, runner, current, "runner-reentry", 1)
				requireNativeStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runner.redemption.Credential, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "runner-reentry", LeaseID: lease.ID, FencingToken: lease.FencingToken}, ExpectedRevision: current.Revision, State: "Merging", Reason: "worker_progress", ReasonDetail: reentryDetail}), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", runner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
			}
			var transition eventstore.Event
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				reader, ok := r.(eventstore.WorkflowTransitionReader)
				if !ok {
					return hubapi.ErrPostgresFeatureUnavailable
				}
				var err error
				transition, err = reader.LatestWorkflowTransition(t.Context(), f.project.ID, issue.WorkItemID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if transition.ActorKind != test.reentry || transition.ToState != "Merging" {
				t.Fatalf("re-entry actor = %q, state = %q, want %s in Merging", transition.ActorKind, transition.ToState, test.reentry)
			}
			if test.publish {
				response := performHubAPIRequest(t, f.service, http.MethodGet, strings.TrimSuffix(path, "/workflow")+"/changes", f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var changes []tracker.ChangeRequest
				decodeHubResponse(t, response, &changes)
				if len(changes) != 1 {
					t.Fatalf("changes = %+v", changes)
				}
				changePath := strings.TrimSuffix(path, "/workflow") + "/changes/" + changes[0].ID
				input := changeTestInput()
				input.PolicyID = f.descriptor.ID
				input.HeadSHA = strings.Repeat("c", 40)
				bundle := changeTestSource(t, &input)
				response = performHubAPIRequest(t, f.service, http.MethodPost, changePath+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "republished"}, ExpectedVersionID: changes[0].CurrentVersion, ChangeVersionInput: input, SourceBundle: bundle})
				requireNativeStatus(t, response, http.StatusOK)
				var version tracker.ChangeVersion
				decodeHubResponse(t, response, &version)
				if _, err := f.service.config.TenantRepository.(*postgres.Repository).BackfillCurrentChangeReviews(t.Context(), f.project.OrganizationID, eventstore.NewContent(f.service.runDetailStore()), corestore.MaxPageSize); err != nil {
					t.Fatal(err)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, changePath+"/versions/"+version.ID+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "approve-republished"}, Decision: "approved"}), http.StatusOK)
			}
			if got := ready(); got != test.want {
				t.Fatalf("landing ready = %t, want %t", got, test.want)
			}
		})
	}
}

func fixtureCollaborationVersion(ctx context.Context, reader *neutralimport.Index, organization, project, item string, revision int64) ([]byte, error) {
	rows, err := reader.Rows(ctx, "collaboration_versions", map[string]string{"organization_id": organization, "project_id": project, "work_item_id": item, "record_id": item}, []string{"revision"}, []string{"0"}, int(revision))
	if err != nil {
		return nil, err
	}
	var raw []byte
	for _, row := range rows {
		var number int64
		var encoding, value string
		if err := row.Decode(map[string]any{"revision": &number, "encoding": &encoding, "record_json": &value}); err != nil {
			return nil, err
		}
		switch encoding {
		case "snapshot":
			raw = []byte(value)
		case "patch":
			raw, err = neutralimport.ApplyVersionPatch(raw, []byte(value))
			if err != nil {
				return nil, err
			}
		default:
			return nil, errors.New("unknown history encoding")
		}
		if number == revision {
			return raw, nil
		}
	}
	return nil, sql.ErrNoRows
}
