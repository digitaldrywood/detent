package hubserver

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestChangeValidatorSessionAuthority(t *testing.T) {
	t.Parallel()
	implementationReview := func(r *tracker.ReviewChange) {
		r.ImplementationReview, r.Validator = r.Validator, nil
	}
	multipleReceipts := func(r *tracker.ReviewChange) {
		r.Validator.CriteriaEvidence[0] = gate.CriterionEvidence{Criterion: "Complete the requested work.", Kind: "receipt", Reference: "make check-app; npx playwright test", ReceiptIDs: []string{"timing_app", "timing_browser"}, HeadSHA: r.Validator.HeadSHA, TreeSHA: "tree", Behavior: "Application and browser checks exercise the requested behavior"}
		r.Validator.Commands = []gate.CommandResult{{ReceiptID: "timing_app", Command: "make check-app", HeadSHA: r.Validator.HeadSHA, TreeSHA: "tree"}, {ReceiptID: "timing_browser", Command: "npx playwright test", HeadSHA: r.Validator.HeadSHA, TreeSHA: "tree"}}
	}
	for _, test := range []struct {
		name   string
		edit   func(*tracker.ReviewChange)
		want   int
		status string
	}{

		{"security audit metered pass", func(_ *tracker.ReviewChange) {}, http.StatusOK, "reviewed"},
		{"security audit stale base", func(r *tracker.ReviewChange) { r.SecurityAudit.Run.BaseSHA = "other" }, http.StatusUnprocessableEntity, "needs_evidence"},
		{"security audit stale head", func(r *tracker.ReviewChange) { r.SecurityAudit.Run.HeadSHA = "other" }, http.StatusUnprocessableEntity, "needs_evidence"},
		{"security audit old reviewer", func(r *tracker.ReviewChange) { r.SecurityAudit.Run.ReviewerDigest = "other" }, http.StatusUnprocessableEntity, "needs_evidence"},
		{"security audit implementing session", func(r *tracker.ReviewChange) { r.SecurityAudit.Run.ID = 41; r.SecurityAudit.SessionID = 41 }, http.StatusUnprocessableEntity, "needs_evidence"},
		{"security audit backend failure", func(r *tracker.ReviewChange) { r.SecurityAudit.Run.ExitStatus = securityaudit.ExitStatusFailed }, http.StatusUnprocessableEntity, "needs_evidence"},
		{"security audit blocking finding", func(r *tracker.ReviewChange) {
			r.SecurityAudit.Run.Verdict = "fail"
			r.SecurityAudit.Run.Findings = []securityaudit.Finding{{ID: "auth", Severity: "p1", Body: "authorization bypass"}}
			r.SecurityAudit.Verdict = "rework"
			r.Decision = "changes_requested"
		}, http.StatusOK, "needs_evidence"},
		{"security audit advisory", func(r *tracker.ReviewChange) {
			r.SecurityAudit.Run.Verdict = "fail"
			r.SecurityAudit.Run.Findings = []securityaudit.Finding{{ID: "advisory", Severity: "p3", Body: "advisory finding"}}
		}, http.StatusOK, "reviewed"},
		{"source review from another machine does not collide", func(_ *tracker.ReviewChange) {}, http.StatusOK, "reviewed"},
		{"same machine source session cannot validate acceptance", func(_ *tracker.ReviewChange) {}, http.StatusUnprocessableEntity, "needs_evidence"},
		{"implementation review is not acceptance", implementationReview, http.StatusOK, "needs_evidence"},
		{"implementation review is not human approval", implementationReview, http.StatusOK, "needs_evidence"},
		{"implementation review does not leave stale human approval", implementationReview, http.StatusOK, "needs_evidence"},
		{"implementation review same session", func(r *tracker.ReviewChange) {
			implementationReview(r)
			r.ImplementationReview.SessionID = 41
		}, http.StatusUnprocessableEntity, "needs_evidence"},
		{"implementation review mixed result", func(r *tracker.ReviewChange) {
			r.ImplementationReview = r.Validator
		}, http.StatusUnprocessableEntity, "needs_evidence"},
		{"separate session pass", func(_ *tracker.ReviewChange) {}, http.StatusOK, "reviewed"},
		{"separate session pass with multiple receipts", multipleReceipts, http.StatusOK, "reviewed"},
		{"failed selected receipt cannot pass", func(r *tracker.ReviewChange) {
			multipleReceipts(r)
			r.Validator.Commands[1].ExitCode = 1
		}, http.StatusUnprocessableEntity, "needs_evidence"},
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
				cfg.Gate.AutomatedReview = gate.AutomatedReviewOff
				cfg.Gate.Validator.UnverifiedCriteria = gate.UnverifiedCriteriaDisclose
				resolved, err := config.ResolvePolicy(config.Workflow{Config: cfg})
				if err != nil {
					t.Fatal(err)
				}
				descriptor = resolved
			}
			audit := strings.HasPrefix(test.name, "security audit")
			descriptor.Gates.Validator = !audit
			descriptor.Gates.SecurityAudit = audit
			implementation := strings.HasPrefix(test.name, "implementation review")
			counterCollision := test.name == "source review from another machine does not collide" || test.name == "same machine source session cannot validate acceptance"
			if implementation || counterCollision {
				descriptor.Gates.AutomatedReview = gate.AutomatedReviewRequired
			}
			humanReview := test.name == "separate session pass with human review" || test.name == "implementation review is not human approval" || test.name == "implementation review does not leave stale human approval"
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
			if audit {
				now := f.service.config.now()
				request.SecurityAudit = &tracker.SecurityAuditReview{SessionID: 42, DiffDigest: policy.Digest([]byte("diff")), Verdict: "pass", Run: securityaudit.Run{ID: 42, InvocationID: "audit-invocation", ProjectID: string(f.project.ID), IssueID: string(issue.WorkItemID), Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(), AuthenticationMode: "api_key", ExitStatus: securityaudit.ExitStatusSuccess, WorkerPID: 1, WorkerPGID: 1, WorkerStartedAt: now, StartedAt: now.Add(-time.Second), CompletedAt: now, OutputBytes: 1, OutputDigest: securityaudit.OutputDigest("result"), Verdict: "pass", Summary: "No findings"}}
				request.Validator = nil
			}
			test.edit(&request)
			if counterCollision {
				machine := tracker.MachineID("machine")
				if test.name == "source review from another machine does not collide" {
					machine = "other-machine"
				}
				sourceReview := tracker.ChangeReview{SessionMachineID: machine, ID: "review_previous_source", VersionID: version.ID, Decision: "approved", ImplementationReview: &gate.ValidatorResult{VersionID: version.ID, Repository: version.Repository, HeadSHA: version.HeadSHA, BaseSHA: version.BaseSHA, SessionID: 42, Submitted: true, Verdict: "pass", Score: 1}, Actor: tracker.Actor{Kind: "runner", PrincipalID: "previous-worker"}, CreatedAt: f.service.config.now()}
				raw, err := marshalNative(sourceReview)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO change_evidence (change_id, version_id, kind, record_json) VALUES (?, ?, 'review', ?)", change.ID, version.ID, raw); err != nil {
					t.Fatal(err)
				}
			}
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
			if test.want == http.StatusOK {
				wantReviews := 1
				if counterCollision {
					wantReviews++
				}
				if len(detail.Reviews) != wantReviews {
					t.Fatalf("review session was not persisted: %+v", detail.Reviews)
				}
				storedReview := detail.Reviews[len(detail.Reviews)-1]
				stored := storedReview.Validator
				if implementation {
					stored = storedReview.ImplementationReview
					if storedReview.Validator != nil {
						t.Fatal("implementation review was stored as acceptance")
					}
				}
				if audit {
					stored = &gate.ValidatorResult{SessionID: storedReview.SecurityAudit.SessionID}
				}
				if stored == nil || stored.SessionID != 42 || storedReview.SessionMachineID != lease.MachineID {
					t.Fatalf("review session was not persisted: %+v", detail.Reviews)
				}
			}
			if test.name == "separate session pass with multiple receipts" {
				stored := detail.Reviews[0].Validator
				if len(stored.CriteriaEvidence) != 1 || stored.CriteriaEvidence[0].Kind != "receipt" || !slices.Equal(stored.CriteriaEvidence[0].ReceiptIDs, []string{"timing_app", "timing_browser"}) || len(stored.NotVerified) != 0 {
					t.Fatalf("receipt selection did not survive native review: %+v", stored)
				}
			}
			if test.name == "missing evidence rework" || test.name == "disclosed missing evidence" {
				stored := detail.Reviews[0].Validator
				if len(stored.CriteriaEvidence) != 1 || stored.CriteriaEvidence[0].Kind != "not_verified" || len(stored.NotVerified) != 1 || stored.NotVerified[0] != "Complete the requested work." {
					t.Fatalf("missing evidence not persisted on its version: %+v", stored)
				}
			}
			if strings.HasPrefix(test.name, "separate session pass") || test.name == "implementation review does not leave stale human approval" {
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
					t.Errorf("old automated review counted as human approval: %+v", current.Summary)
				}
				response = performHubAPIRequest(t, f.service, http.MethodGet, path+"?view=bounded&section=current", f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var bounded operatortool.ChangeResult
				decodeHubResponse(t, response, &bounded)
				if bounded.Detail == nil || bounded.Detail.Summary.NativeReview != review {
					t.Fatalf("bounded read attributed automated review to a human: %+v", bounded.Detail)
				}

			}
		})
	}
}
