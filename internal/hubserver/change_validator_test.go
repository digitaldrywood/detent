package hubserver

import (
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestChangeValidatorSessionAuthority(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		edit   func(*tracker.ReviewChange)
		want   int
		status string
	}{
		{"separate session pass", func(_ *tracker.ReviewChange) {}, http.StatusOK, "reviewed"},
		{"separate session pass with human review", func(_ *tracker.ReviewChange) {}, http.StatusOK, "needs_evidence"},
		{"implementing session", func(r *tracker.ReviewChange) { r.Validator.SessionID = 41 }, http.StatusUnprocessableEntity, "needs_evidence"},
		{"missing session", func(r *tracker.ReviewChange) { r.Validator.SessionID = 0 }, http.StatusUnprocessableEntity, "needs_evidence"},
		{"another head", func(r *tracker.ReviewChange) { r.Validator.HeadSHA = r.Validator.BaseSHA }, http.StatusUnprocessableEntity, "needs_evidence"},
		{"another version", func(r *tracker.ReviewChange) { r.Validator.VersionID = "version_other" }, http.StatusUnprocessableEntity, "needs_evidence"},
		{"human approval from worker", func(r *tracker.ReviewChange) { r.Validator = nil }, http.StatusForbidden, "needs_evidence"},
		{"verdict mismatch", func(r *tracker.ReviewChange) { r.Validator.Verdict = "rework" }, http.StatusUnprocessableEntity, "needs_evidence"},
		{"separate session rework", func(r *tracker.ReviewChange) { r.Decision, r.Validator.Verdict = "changes_requested", "rework" }, http.StatusOK, "needs_evidence"},
		{"separate session wait", func(r *tracker.ReviewChange) { r.Decision, r.Validator.Verdict = "commented", "wait" }, http.StatusOK, "needs_evidence"},
		{"missing evidence cannot pass", func(r *tracker.ReviewChange) { r.Validator.CriteriaEvidence = nil }, http.StatusUnprocessableEntity, "needs_evidence"},
		{"missing evidence rework", func(r *tracker.ReviewChange) {
			r.Validator.CriteriaEvidence = nil
			r.Decision, r.Validator.Verdict = "changes_requested", "rework"
		}, http.StatusOK, "needs_evidence"},
		{"disclosed missing evidence", func(r *tracker.ReviewChange) { r.Validator.CriteriaEvidence = nil }, http.StatusOK, "reviewed"},
		{"restated evidence cannot pass", func(r *tracker.ReviewChange) { r.Validator.CriteriaEvidence[0].RestatesImplementation = true }, http.StatusUnprocessableEntity, "needs_evidence"},
		{"mismatched receipt cannot pass", func(r *tracker.ReviewChange) {
			r.Validator.CriteriaEvidence[0] = gate.CriterionEvidence{Criterion: "Complete the requested work.", Kind: "receipt", Reference: "make check", HeadSHA: r.Validator.HeadSHA, TreeSHA: "other", Behavior: "The configured check proves the requested behavior"}
			r.Validator.Commands = []gate.CommandResult{{Command: "make check", HeadSHA: r.Validator.HeadSHA, TreeSHA: "tree"}}
		}, http.StatusUnprocessableEntity, "needs_evidence"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "validator")
			descriptor := hubTestPolicy()
			if test.name == "disclosed missing evidence" {
				cfg := config.Default()
				cfg.Tracker.Kind = config.TrackerHubNative
				cfg.Gate.Validator.Enabled = true
				cfg.Gate.Validator.UnverifiedCriteria = gate.UnverifiedCriteriaDisclose
				resolved, err := config.ResolvePolicy(config.Workflow{Config: cfg})
				if err != nil {
					t.Fatal(err)
				}
				descriptor = resolved
			}
			descriptor.Gates.Validator = true
			humanReview := test.name == "separate session pass with human review"
			if humanReview {
				descriptor.Gates.HumanReview = true
			}
			descriptor = descriptor.WithID()
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			issue := f.create(t, "work")
			worker := f.worker(t, "worker")
			lease := claimNativeAttempt(t, f, worker, "machine", "session", issue.WorkItemID, descriptor.ID)
			event := nativeStartedEvent(lease)
			event.Data.Identity.Role = "code"
			event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", Activity: &workflowmetrics.ActivityProfile{Schema: 1, SessionID: 41}}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, event), http.StatusOK)
			mutation := tracker.Mutation{IdempotencyKey: "change", LeaseID: lease.ID, FencingToken: lease.FencingToken}
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/changes"
			response := performHubAPIRequest(t, f.service, http.MethodPost, path, worker, tracker.CreateChange{Mutation: mutation, Title: "Validate immutable change"})
			requireNativeStatus(t, response, http.StatusOK)
			var change tracker.ChangeRequest
			decodeHubResponse(t, response, &change)
			path += "/" + change.ID
			input := changeTestInput()
			input.PolicyID, input.RunID, input.AttemptID = descriptor.ID, event.Data.RunID, event.Data.AttemptID
			mutation.IdempotencyKey = "publish"
			response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions", worker, tracker.PublishChangeVersion{Mutation: mutation, ChangeVersionInput: input})
			requireNativeStatus(t, response, http.StatusOK)
			var version tracker.ChangeVersion
			decodeHubResponse(t, response, &version)
			mutation.IdempotencyKey = "validator"
			request := tracker.ReviewChange{Mutation: mutation, ExpectedVersionID: version.ID, Decision: "approved", Validator: &gate.ValidatorResult{SessionID: 42, VersionID: version.ID, Submitted: true, Verdict: "pass", Score: .95, Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, DiffDigest: policy.Digest([]byte("diff"))}}
			request.Validator.CriteriaEvidence = []gate.CriterionEvidence{{Criterion: "Complete the requested work.", Kind: "test", Reference: "change_validator_test.go:TestChangeValidatorSessionAuthority", Behavior: "A separate validator session reviews the immutable version"}}
			test.edit(&request)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", worker, request), test.want)
			response = performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var detail tracker.ChangeDetail
			decodeHubResponse(t, response, &detail)
			review := "not_required"
			if humanReview {
				review = "pending"
			}
			if detail.Summary.Status != test.status || detail.Summary.NativeReview != review {
				t.Fatalf("validator summary = %+v, want %s", detail.Summary, test.status)
			}
			current, err := readCurrentChangeDetail(t.Context(), f.service.database.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, detail.Change, f.service.config.now())
			if err != nil || current.Summary.Status != detail.Summary.Status {
				t.Fatalf("current summary differs: %+v, %v", current.Summary, err)
			}
			if test.want == http.StatusOK && (len(detail.Reviews) != 1 || detail.Reviews[0].Validator == nil || detail.Reviews[0].Validator.SessionID != 42) {
				t.Fatalf("validator session was not persisted: %+v", detail.Reviews)
			}
			if test.name == "missing evidence rework" || test.name == "disclosed missing evidence" {
				stored := detail.Reviews[0].Validator
				if len(stored.CriteriaEvidence) != 1 || stored.CriteriaEvidence[0].Kind != "not_verified" || len(stored.NotVerified) != 1 || stored.NotVerified[0] != "Complete the requested work." {
					t.Fatalf("missing evidence not persisted on its version: %+v", stored)
				}
			}
			if strings.HasPrefix(test.name, "separate session pass") {
				input.HeadSHA = strings.Repeat("c", 40)
				mutation.IdempotencyKey = "publish-next"
				response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions", worker, tracker.PublishChangeVersion{Mutation: mutation, ExpectedVersionID: version.ID, ChangeVersionInput: input})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &version)
				response = performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
				decodeHubResponse(t, response, &detail)
				current, err = readCurrentChangeDetail(t.Context(), f.service.database.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, detail.Change, f.service.config.now())
				if err != nil || current.Summary.Status != "needs_evidence" || current.Summary.Status != detail.Summary.Status {
					t.Fatalf("old validator approved new version: %+v, %v", current.Summary, err)
				}
				if current.Summary.NativeReview != review {
					t.Fatalf("old validator counted as human approval: %+v", current.Summary)
				}

			}
		})
	}
}
