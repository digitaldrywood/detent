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
	landingAudit := func(r *tracker.ReviewChange) {
		r.SecurityAudit.Landing = true
		r.SecurityAudit.VersionID = r.ExpectedVersionID
		r.SecurityAudit.Run.BaseSHA = strings.Repeat("d", 40)
		r.SecurityAudit.Run.HeadSHA = strings.Repeat("e", 40)
	}
	multipleReceipts := func(r *tracker.ReviewChange) {
		r.Validator.CriteriaEvidence[0] = gate.CriterionEvidence{Criterion: "Complete the requested work.", Kind: "receipt", Reference: "make check-app; npx playwright test", ReceiptIDs: []string{"timing_app", "timing_browser"}, Behavior: "Application and browser checks exercise the requested behavior"}
		r.Validator.Commands = []gate.CommandResult{{ReceiptID: "timing_app", Command: "make check-app", HeadSHA: r.Validator.HeadSHA, TreeSHA: strings.Repeat("d", 40)}, {ReceiptID: "timing_browser", Command: "npx playwright test", HeadSHA: r.Validator.HeadSHA, TreeSHA: strings.Repeat("d", 40)}}
	}
	type stageCase struct {
		mode               string
		gateKind           string
		sourceVerdicts     []string
		retainedAcceptance bool
		freshAcceptance    bool
	}
	for _, test := range []struct {
		name   string
		edit   func(*tracker.ReviewChange)
		want   int
		status string
		stages *stageCase
	}{
		{"required acceptance before source", func(_ *tracker.ReviewChange) {}, http.StatusUnprocessableEntity, "needs_evidence", &stageCase{mode: gate.AutomatedReviewRequired}},
		{"required acceptance after source pass", func(_ *tracker.ReviewChange) {}, http.StatusOK, "reviewed", &stageCase{mode: gate.AutomatedReviewRequired, sourceVerdicts: []string{"pass"}}},
		{"required acceptance after source rework", func(_ *tracker.ReviewChange) {}, http.StatusUnprocessableEntity, "needs_evidence", &stageCase{mode: gate.AutomatedReviewRequired, sourceVerdicts: []string{"rework"}}},
		{"required acceptance after source wait", func(_ *tracker.ReviewChange) {}, http.StatusUnprocessableEntity, "needs_evidence", &stageCase{mode: gate.AutomatedReviewRequired, sourceVerdicts: []string{"wait"}}},
		{"required acceptance after newer source wait", func(_ *tracker.ReviewChange) {}, http.StatusUnprocessableEntity, "needs_evidence", &stageCase{mode: gate.AutomatedReviewRequired, sourceVerdicts: []string{"pass", "wait"}}},
		{"required acceptance after recovered source", func(_ *tracker.ReviewChange) {}, http.StatusOK, "reviewed", &stageCase{mode: gate.AutomatedReviewRequired, sourceVerdicts: []string{"wait", "rework", "pass"}}},
		{"implementation review retains old acceptance pending", implementationReview, http.StatusOK, "needs_evidence", &stageCase{mode: gate.AutomatedReviewRequired, retainedAcceptance: true}},
		{"implementation review then fresh acceptance replaces retained approval", implementationReview, http.StatusOK, "needs_evidence", &stageCase{mode: gate.AutomatedReviewRequired, retainedAcceptance: true, freshAcceptance: true}},
		{"optional source permits acceptance", func(_ *tracker.ReviewChange) {}, http.StatusOK, "reviewed", &stageCase{mode: gate.AutomatedReviewOptional}},
		{"source review from another machine does not collide", func(_ *tracker.ReviewChange) {}, http.StatusOK, "reviewed", nil},
		{"same machine source session cannot validate acceptance", func(_ *tracker.ReviewChange) {}, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"implementation review is not acceptance", implementationReview, http.StatusOK, "needs_evidence", nil},
		{"implementation review is not human approval", implementationReview, http.StatusOK, "needs_evidence", nil},
		{"implementation review does not leave stale human approval", implementationReview, http.StatusOK, "needs_evidence", nil},
		{"security audit metered pass", func(_ *tracker.ReviewChange) {}, http.StatusOK, "reviewed", nil},
		{"security audit stale base", func(r *tracker.ReviewChange) { r.SecurityAudit.Run.BaseSHA = "other" }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"security audit stale head", func(r *tracker.ReviewChange) { r.SecurityAudit.Run.HeadSHA = "other" }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"security audit old reviewer", func(r *tracker.ReviewChange) { r.SecurityAudit.Run.ReviewerDigest = "other" }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"security audit implementing session", func(r *tracker.ReviewChange) { r.SecurityAudit.Run.ID = 41; r.SecurityAudit.SessionID = 41 }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"security audit backend failure", func(r *tracker.ReviewChange) { r.SecurityAudit.Run.ExitStatus = securityaudit.ExitStatusFailed }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"security audit landing pass", landingAudit, http.StatusOK, "reviewed", nil},
		{"security audit landing requires merge execution", landingAudit, http.StatusUnprocessableEntity, "reviewed", nil},
		{"security audit landing stale version", func(r *tracker.ReviewChange) { landingAudit(r); r.SecurityAudit.VersionID = "other" }, http.StatusUnprocessableEntity, "reviewed", nil},
		{"security audit landing backend failure", func(r *tracker.ReviewChange) {
			landingAudit(r)
			r.SecurityAudit.Run.ExitStatus = securityaudit.ExitStatusFailed
		}, http.StatusUnprocessableEntity, "reviewed", nil},
		{"security audit landing blocking finding", func(r *tracker.ReviewChange) {
			landingAudit(r)
			r.SecurityAudit.Run.Verdict = "fail"
			r.SecurityAudit.Run.Findings = []securityaudit.Finding{{ID: "auth", Severity: "p1", Body: "Authorization bypass"}}
			r.SecurityAudit.Verdict = "rework"
			r.Decision = "changes_requested"
		}, http.StatusOK, "needs_evidence", nil},
		{"security audit blocking finding", func(r *tracker.ReviewChange) {
			r.SecurityAudit.Run.Verdict = "fail"
			r.SecurityAudit.Run.Findings = []securityaudit.Finding{{ID: "auth", Severity: "p1", Body: "authorization bypass"}}
			r.SecurityAudit.Verdict = "rework"
			r.Decision = "changes_requested"
		}, http.StatusOK, "needs_evidence", nil},
		{"security audit advisory", func(r *tracker.ReviewChange) {
			r.SecurityAudit.Run.Verdict = "fail"
			r.SecurityAudit.Run.Findings = []securityaudit.Finding{{ID: "advisory", Severity: "p3", Body: "advisory finding"}}
		}, http.StatusOK, "reviewed", nil},
		{"implementation review same session", func(r *tracker.ReviewChange) {
			implementationReview(r)
			r.ImplementationReview.SessionID = 41
		}, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"implementation review mixed result", func(r *tracker.ReviewChange) {
			r.ImplementationReview = r.Validator
		}, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"separate session pass", func(_ *tracker.ReviewChange) {}, http.StatusOK, "reviewed", nil},
		{"separate session pass with multiple receipts", multipleReceipts, http.StatusOK, "reviewed", nil},
		{"artifact gate receipt without finalization", multipleReceipts, http.StatusOK, "reviewed", &stageCase{mode: gate.AutomatedReviewOff, gateKind: gate.KindArtifact}},
		{"human gate receipt without finalization", multipleReceipts, http.StatusOK, "needs_evidence", &stageCase{mode: gate.AutomatedReviewOff, gateKind: gate.KindHumanReview}},
		{"failed selected receipt cannot pass", func(r *tracker.ReviewChange) {
			multipleReceipts(r)
			r.Validator.Commands[1].ExitCode = 1
		}, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"separate session pass with human review", func(_ *tracker.ReviewChange) {}, http.StatusOK, "needs_evidence", nil},
		{"implementing session", func(r *tracker.ReviewChange) { r.Validator.SessionID = 41 }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"missing session", func(r *tracker.ReviewChange) { r.Validator.SessionID = 0 }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"another head", func(r *tracker.ReviewChange) { r.Validator.HeadSHA = r.Validator.BaseSHA }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"another version", func(r *tracker.ReviewChange) { r.Validator.VersionID = "version_other" }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"human approval from worker", func(r *tracker.ReviewChange) { r.Validator = nil }, http.StatusForbidden, "needs_evidence", nil},
		{"verdict mismatch", func(r *tracker.ReviewChange) { r.Validator.Verdict = "rework" }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"separate session rework", func(r *tracker.ReviewChange) { r.Decision, r.Validator.Verdict = "changes_requested", "rework" }, http.StatusOK, "needs_evidence", nil},
		{"separate session wait", func(r *tracker.ReviewChange) { r.Decision, r.Validator.Verdict = "commented", "wait" }, http.StatusOK, "needs_evidence", nil},
		{"missing evidence cannot pass", func(r *tracker.ReviewChange) { r.Validator.CriteriaEvidence = nil }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"missing evidence rework", func(r *tracker.ReviewChange) {
			r.Validator.CriteriaEvidence = nil
			r.Decision, r.Validator.Verdict = "changes_requested", "rework"
		}, http.StatusOK, "needs_evidence", nil},
		{"disclosed missing evidence", func(r *tracker.ReviewChange) { r.Validator.CriteriaEvidence = nil }, http.StatusOK, "reviewed", nil},
		{"restated evidence cannot pass", func(r *tracker.ReviewChange) { r.Validator.CriteriaEvidence[0].RestatesImplementation = true }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"mismatched receipt cannot pass", func(r *tracker.ReviewChange) {
			r.Validator.CriteriaEvidence[0] = gate.CriterionEvidence{Criterion: "Complete the requested work.", Kind: "receipt", Reference: "make check", HeadSHA: r.Validator.HeadSHA, TreeSHA: "other", Behavior: "The configured check proves the requested behavior"}
			r.Validator.Commands = []gate.CommandResult{{Command: "make check", HeadSHA: r.Validator.HeadSHA, TreeSHA: "tree"}}
		}, http.StatusUnprocessableEntity, "needs_evidence", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "validator")
			descriptor := hubTestPolicy()
			if test.stages != nil && test.stages.gateKind != "" {
				cfg := config.Default()
				cfg.Tracker.Kind = config.TrackerHubNative
				cfg.Gate.Kind = test.stages.gateKind
				cfg.Gate.Validator.Enabled = true
				cfg.Gate.SecurityAudit.Enabled = false
				cfg.Review.Human = test.stages.gateKind == gate.KindHumanReview
				resolved, err := config.ResolvePolicy(config.Workflow{Config: cfg})
				if err != nil {
					t.Fatal(err)
				}
				descriptor = resolved
				if gate.Effective(cfg.Gate).Run != "" {
					t.Fatal("non-command gate must omit source finalization validation")
				}
			}
			if test.name == "disclosed missing evidence" {
				cfg := config.Default()
				cfg.Tracker.Kind = config.TrackerHubNative
				cfg.Gate.Validator.Enabled = true
				cfg.Gate.SecurityAudit.Enabled = false
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
			if test.stages != nil && test.stages.gateKind == "" {
				descriptor.Gates.AutomatedReview = test.stages.mode
			}
			humanReview := test.name == "separate session pass with human review" || test.name == "implementation review is not human approval" || test.name == "implementation review does not leave stale human approval" || test.stages != nil && test.stages.gateKind == gate.KindHumanReview
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
			if test.name == "separate session pass with multiple receipts" || test.name == "failed selected receipt cannot pass" {
				input.Validation = &gate.CommandResult{Command: "make check-land", HeadSHA: input.HeadSHA, TreeSHA: strings.Repeat("d", 40), DurationNS: 1}
			}
			mutation.IdempotencyKey = "publish"
			response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions", worker, tracker.PublishChangeVersion{Mutation: mutation, ChangeVersionInput: input})
			requireNativeStatus(t, response, http.StatusOK)
			var version tracker.ChangeVersion
			decodeHubResponse(t, response, &version)
			if test.stages != nil && test.stages.gateKind != "" && version.Validation != nil {
				t.Fatal("non-command fixture unexpectedly has a finalization receipt")
			}
			mutation.IdempotencyKey = "validator"
			request := tracker.ReviewChange{Mutation: mutation, ExpectedVersionID: version.ID, Decision: "approved", Validator: &gate.ValidatorResult{SessionID: 42, VersionID: version.ID, Submitted: true, Verdict: "pass", Score: .95, Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, DiffDigest: policy.Digest([]byte("diff"))}}
			request.Validator.CriteriaEvidence = []gate.CriterionEvidence{{Criterion: "Complete the requested work.", Kind: "test", Reference: "change_validator_test.go:TestChangeValidatorSessionAuthority", Behavior: "A separate validator session reviews the immutable version"}}
			acceptanceRequest := request
			if audit {
				now := f.service.config.now()
				request.SecurityAudit = &tracker.SecurityAuditReview{SessionID: 42, DiffDigest: policy.Digest([]byte("diff")), Verdict: "pass", Run: securityaudit.Run{ID: 42, InvocationID: "audit-invocation", ProjectID: string(f.project.ID), IssueID: string(issue.WorkItemID), Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(), AuthenticationMode: "api_key", ExitStatus: securityaudit.ExitStatusSuccess, WorkerPID: 1, WorkerPGID: 1, WorkerStartedAt: now, StartedAt: now.Add(-time.Second), CompletedAt: now, OutputBytes: 1, OutputDigest: securityaudit.OutputDigest("result"), Verdict: "pass", Summary: "No findings"}}
				request.Validator = nil
			}
			landing := strings.HasPrefix(test.name, "security audit landing")
			if landing {
				source := request
				sourceAudit := *request.SecurityAudit
				sourceAudit.SessionID, sourceAudit.Run.ID = 43, 43
				sourceAudit.Run.InvocationID = "source-audit"
				source.SecurityAudit = &sourceAudit
				source.IdempotencyKey = "source-audit"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", worker, source), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
				lease = claimNativeAttempt(t, f, worker, "machine", "merge-session", issue.WorkItemID, descriptor.ID)
				merge := nativeStartedEvent(lease)
				merge.IdempotencyKey = "merge-start"
				merge.Data.Identity.Role = "merge"
				if test.name == "security audit landing requires merge execution" {
					merge.Data.Identity.Role = "code"
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, merge), http.StatusOK)
				request.LeaseID, request.FencingToken = lease.ID, lease.FencingToken
			}
			test.edit(&request)
			stageReviews := 0
			if test.stages != nil {
				if test.stages.retainedAcceptance {
					retained := *acceptanceRequest.Validator
					retained.SessionID = 44
					raw, err := marshalNative(tracker.ChangeReview{ID: "review_retained_acceptance", VersionID: version.ID, Decision: "approved", Validator: &retained, SessionMachineID: lease.MachineID, Actor: tracker.Actor{Kind: "runner", PrincipalID: "previous-worker"}, CreatedAt: f.service.config.now()})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO change_evidence (change_id, version_id, kind, record_json) VALUES (?, ?, 'review', ?)", change.ID, version.ID, raw); err != nil {
						t.Fatal(err)
					}
					stageReviews++
				}
				for i, verdict := range test.stages.sourceVerdicts {
					source := *acceptanceRequest.Validator
					source.SessionID, source.Verdict = int64(50+i), verdict
					sourceRequest := tracker.ReviewChange{Mutation: mutation, ExpectedVersionID: version.ID, ImplementationReview: &source, Decision: "approved"}
					sourceRequest.IdempotencyKey = "source-" + verdict
					switch verdict {
					case "wait":
						sourceRequest.Decision = "commented"
					case "rework":
						sourceRequest.Decision = "changes_requested"
					}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", worker, sourceRequest), http.StatusOK)
					stageReviews++
				}
			}
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
			wantSession := int64(42)
			if test.stages != nil && test.stages.freshAcceptance {
				fresh := *acceptanceRequest.Validator
				fresh.SessionID = 43
				acceptanceRequest.Validator = &fresh
				acceptanceRequest.IdempotencyKey = "fresh-acceptance"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", worker, acceptanceRequest), http.StatusOK)
				response = performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &detail)
				if detail.Summary.Status != "reviewed" {
					t.Fatalf("fresh acceptance did not complete ordered reviews: %+v", detail.Summary)
				}
				stageReviews++
				wantSession = 43
			}
			current, err := readCurrentChangeDetail(t.Context(), f.service.database.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, detail.Change, f.service.config.now())
			if err != nil || current.Summary.Status != detail.Summary.Status {
				t.Fatalf("current summary differs: %+v, %v", current.Summary, err)
			}
			if test.want == http.StatusOK {
				wantReviews := 1 + stageReviews
				if counterCollision || landing {
					wantReviews++
				}
				if len(detail.Reviews) != wantReviews {
					t.Fatalf("review session was not persisted: %+v", detail.Reviews)
				}
				storedReview := detail.Reviews[len(detail.Reviews)-1]
				stored := storedReview.Validator
				if implementation && wantSession == 42 {
					stored = storedReview.ImplementationReview
					if storedReview.Validator != nil {
						t.Fatal("implementation review was stored as acceptance")
					}
				}
				if audit {
					stored = &gate.ValidatorResult{SessionID: storedReview.SecurityAudit.SessionID}
				}
				if stored == nil || stored.SessionID != wantSession || storedReview.SessionMachineID != lease.MachineID {
					t.Fatalf("review session was not persisted: %+v", detail.Reviews)
				}
			}
			if test.name == "security audit landing pass" {
				original := request.SecurityAudit.Run
				for _, mismatch := range []string{"base", "head", "unrecorded", "modified", "none", "pass"} {
					audit := *request.SecurityAudit
					receipt := &tracker.NativeLandingReceipt{SecurityAudit: &audit, BaseSHA: original.BaseSHA, MergeSHA: original.HeadSHA}
					want := http.StatusUnprocessableEntity
					switch mismatch {
					case "base":
						receipt.BaseSHA = strings.Repeat("f", 40)
					case "head":
						audit.Run.HeadSHA = strings.Repeat("f", 40)
					case "unrecorded":
						audit.Run.InvocationID = "unrecorded"
					case "modified":
						audit.Run.Summary = "Modified finding evidence"
					case "none":
						receipt.SecurityAudit = nil
					case "pass":
						want = http.StatusOK
					}
					land := tracker.LandChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "landing-" + mismatch, LeaseID: lease.ID, FencingToken: lease.FencingToken}, Receipt: receipt, MergeSHA: original.HeadSHA, BaseRef: "main", Method: "squash"}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/landing", worker, land), want)
				}
			}
			if test.name == "security audit blocking finding" || test.name == "security audit landing blocking finding" {
				passing := *request.SecurityAudit
				passing.SessionID, passing.Run.ID = 44, 44
				passing.Run.InvocationID = "later-pass"
				passing.Run.Verdict, passing.Verdict = "pass", "pass"
				passing.Run.Findings = nil
				request.SecurityAudit = &passing
				request.Decision = "approved"
				request.IdempotencyKey = "later-audit"
				response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", worker, request)
				requireNativeStatus(t, response, http.StatusOK)
				response = performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &detail)
				if detail.Summary.Status != "needs_evidence" || len(detail.Reviews) < 2 || detail.Reviews[len(detail.Reviews)-2].Decision != "changes_requested" {
					t.Fatalf("later pass cleared the unchanged version's blocking audit: %+v", detail)
				}
			}
			if test.name == "separate session pass with multiple receipts" || test.stages != nil && test.stages.gateKind != "" {
				stored := detail.Reviews[0].Validator
				if len(stored.CriteriaEvidence) != 1 || stored.CriteriaEvidence[0].Kind != "receipt" || !slices.Equal(stored.CriteriaEvidence[0].ReceiptIDs, []string{"timing_app", "timing_browser"}) || stored.CriteriaEvidence[0].HeadSHA != version.HeadSHA || stored.CriteriaEvidence[0].TreeSHA != strings.Repeat("d", 40) || len(stored.NotVerified) != 0 {
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
				if input.Validation != nil {
					input.Validation.HeadSHA = input.HeadSHA
				}
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
