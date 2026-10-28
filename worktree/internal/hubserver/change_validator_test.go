package hubserver

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/failureattribution"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestChangeValidatorSessionAuthority(t *testing.T) {
	t.Parallel()
	attributedRework := func(r *tracker.ReviewChange) {
		result := r.Validator
		r.Decision, result.Verdict = "changes_requested", "rework"
		diagnostic := "source.go:4: invalid input"
		base := failureattribution.Observation{Command: "check-input", HeadSHA: result.BaseSHA, ExitCode: 1, Output: diagnostic, Complete: true}
		result.Commands = []gate.CommandResult{{Command: base.Command, HeadSHA: result.HeadSHA, ExitCode: 1, Output: diagnostic, FinishedAt: time.Now(), BaseReproduction: &base}}
		result.Findings = []gate.Finding{{Severity: "p1", Body: diagnostic, CheckCommand: base.Command, CheckDiagnostic: diagnostic}}
	}
	implementationReview := func(r *tracker.ReviewChange) {
		r.ImplementationReview, r.Validator = r.Validator, nil
	}
	blockingAudit := func(r *tracker.ReviewChange) {
		r.SecurityAudit.Run.Verdict = "fail"
		r.SecurityAudit.Run.Findings = []securityaudit.Finding{{ID: "auth", Severity: "p1", Body: "authorization bypass"}}
		r.SecurityAudit.Verdict = "rework"
		r.Decision = "changes_requested"
	}
	landingAudit := func(r *tracker.ReviewChange) {
		r.SecurityAudit.Landing = true
		r.SecurityAudit.VersionID = r.ExpectedVersionID
		r.SecurityAudit.Run.BaseSHA = strings.Repeat("d", 40)
		r.SecurityAudit.Run.HeadSHA = strings.Repeat("e", 40)
	}
	largeFindingIdentities := func(r *tracker.ReviewChange) {
		for i := range 5800 {
			r.SecurityAudit.Run.Findings = append(r.SecurityAudit.Run.Findings, securityaudit.Finding{ID: fmt.Sprintf("finding-%06d-%s", i, strings.Repeat("i", 64)), Status: "resolved", Severity: "p1", Body: "Resolved"})
		}
	}
	multipleReceipts := func(r *tracker.ReviewChange) {
		r.Validator.CriteriaEvidence[0] = gate.CriterionEvidence{Criterion: "Complete the requested work.", Kind: "receipt", Reference: "make check-app; npx playwright test", ReceiptIDs: []string{"timing_app", "timing_browser"}, Behavior: "Application and browser checks exercise the requested behavior"}
		r.Validator.Commands = []gate.CommandResult{{ReceiptID: "timing_app", Command: "make check-app", HeadSHA: r.Validator.HeadSHA, TreeSHA: strings.Repeat("d", 40)}, {ReceiptID: "timing_browser", Command: "npx playwright test", HeadSHA: r.Validator.HeadSHA, TreeSHA: strings.Repeat("d", 40)}}
	}
	objectOutput := func(r *tracker.ReviewChange) {
		r.Validator.Commands = []gate.CommandResult{{Command: "make check-land", HeadSHA: r.Validator.HeadSHA, TreeSHA: strings.Repeat("d", 40), DurationNS: 123, Output: strings.Repeat("validator output\n", 13000)}}
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
		{"visual criterion paired upload", func(r *tracker.ReviewChange) {
			r.Validator.CriteriaEvidence[0].Reference = "https://figma.com/design/example ![upload](attachment:att_0123456789abcdef0123456789abcdef)"
		}, http.StatusOK, "reviewed", nil},
		{"object-backed visual body refusal", func(r *tracker.ReviewChange) {
			objectOutput(r)
			r.Body = "https://figma.com/design/example?secret=hidden"
		}, http.StatusUnprocessableEntity, "needs_evidence", nil},

		{"visual review body refused", func(r *tracker.ReviewChange) { r.Body = "https://figma.com/design/example?secret=hidden" }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"visual criterion reference refused", func(r *tracker.ReviewChange) {
			r.Validator.CriteriaEvidence[0].Reference = "https://figma.com/design/example?secret=hidden"
		}, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"visual discarded criterion reference refused", func(r *tracker.ReviewChange) {
			r.Validator.CriteriaEvidence = append(r.Validator.CriteriaEvidence, gate.CriterionEvidence{Criterion: "Unrecognized criterion", Kind: "test", Reference: "https://figma.com/design/example?secret=hidden"})
		}, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"implementation review visual reference refused", func(r *tracker.ReviewChange) {
			implementationReview(r)
			r.ImplementationReview.CriteriaEvidence[0].Reference = "https://figma.com/design/example?secret=hidden"
		}, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"visual raw command output preserved", func(r *tracker.ReviewChange) {
			r.Validator.Commands = []gate.CommandResult{{Command: "inspect", Output: "https://figma.com/design/example?secret=hidden"}}
		}, http.StatusOK, "reviewed", nil},

		{"attempt changes during preparation", func(_ *tracker.ReviewChange) {}, http.StatusConflict, "needs_evidence", nil},
		{"security audit attempt changes during preparation", func(_ *tracker.ReviewChange) {}, http.StatusConflict, "needs_evidence", nil},
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
		{"security audit undetermined lead waits for human", func(r *tracker.ReviewChange) {
			r.SecurityAudit.Run.Verdict = securityaudit.VerdictUndetermined
			r.SecurityAudit.Run.Findings = []securityaudit.Finding{{ID: "auth", Certainty: securityaudit.CertaintyUndetermined, Body: "Potential p2 cross-tenant access", UnresolvedFact: "Whether the caller validates tenant authorization"}}
			r.SecurityAudit.Verdict = "wait"
			r.Decision = "commented"
		}, http.StatusOK, "needs_evidence", nil},
		{"security audit rejected lead is retained and passes", func(r *tracker.ReviewChange) {
			r.SecurityAudit.Run.Findings = []securityaudit.Finding{{ID: "auth", Certainty: securityaudit.CertaintyDisproved, Status: "rejected", Body: "Caller enforces tenant authorization"}}
		}, http.StatusOK, "reviewed", nil},
		{"security audit large finding identities", largeFindingIdentities, http.StatusOK, "reviewed", nil},
		{"security audit landing large identities", func(r *tracker.ReviewChange) {
			landingAudit(r)
			largeFindingIdentities(r)
		}, http.StatusOK, "reviewed", nil},
		{"security audit landing shared identities", func(r *tracker.ReviewChange) {
			landingAudit(r)
			largeFindingIdentities(r)
			r.SecurityAudit.Run.Findings = r.SecurityAudit.Run.Findings[:300]
		}, http.StatusOK, "reviewed", nil},
		{"security audit landing large blocking identities", func(r *tracker.ReviewChange) {
			landingAudit(r)
			blockingAudit(r)
			largeFindingIdentities(r)
		}, http.StatusOK, "needs_evidence", nil},
		{"security audit large summary", func(r *tracker.ReviewChange) {
			r.SecurityAudit.Run.Summary = strings.Repeat("s", (1<<20)-(16<<10))
		}, http.StatusOK, "reviewed", nil},
		{"security audit large blocking findings", func(r *tracker.ReviewChange) {
			blockingAudit(r)
			r.SecurityAudit.Run.Findings[0].Body = strings.Repeat("f", (1<<20)-(16<<10))
			r.SecurityAudit.Run.Findings = append(r.SecurityAudit.Run.Findings, securityaudit.Finding{ID: "resolved", Status: "resolved", Severity: "p1", Body: "Already addressed"})
		}, http.StatusOK, "needs_evidence", nil},
		{"security audit metered pass", func(_ *tracker.ReviewChange) {}, http.StatusOK, "reviewed", nil},
		{"security audit run-detail backed pass", func(_ *tracker.ReviewChange) {}, http.StatusOK, "reviewed", nil},
		{"security audit before required implementation review", func(_ *tracker.ReviewChange) {}, http.StatusOK, "needs_evidence", &stageCase{mode: gate.AutomatedReviewRequired}},
		{"security audit blocking finding before required implementation review", blockingAudit, http.StatusOK, "needs_evidence", &stageCase{mode: gate.AutomatedReviewRequired}},
		{"security audit stale head before required implementation review", func(r *tracker.ReviewChange) { r.SecurityAudit.Run.HeadSHA = "other" }, http.StatusUnprocessableEntity, "needs_evidence", &stageCase{mode: gate.AutomatedReviewRequired}},
		{"security audit OS identity clock precedes audit clock", func(r *tracker.ReviewChange) {
			r.SecurityAudit.Run.WorkerStartedAt = r.SecurityAudit.Run.StartedAt.Add(-2 * time.Second)
		}, http.StatusOK, "reviewed", nil},
		{"security audit OS identity clock follows audit clock", func(r *tracker.ReviewChange) {
			r.SecurityAudit.Run.WorkerStartedAt = r.SecurityAudit.Run.CompletedAt.Add(2 * time.Second)
		}, http.StatusOK, "reviewed", nil},
		{"security audit missing run", func(r *tracker.ReviewChange) { r.SecurityAudit.Run.ID = 0 }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"security audit missing session", func(r *tracker.ReviewChange) { r.SecurityAudit.SessionID = 0 }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"security audit foreign issue", func(r *tracker.ReviewChange) { r.SecurityAudit.Run.IssueID = "wi_other" }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"security audit foreign project", func(r *tracker.ReviewChange) { r.SecurityAudit.Run.ProjectID = "prj_other" }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"security audit foreign session", func(r *tracker.ReviewChange) { r.SecurityAudit.Run.ID = 99 }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"security audit p2 finding", func(r *tracker.ReviewChange) {
			r.SecurityAudit.Run.Verdict = "fail"
			r.SecurityAudit.Run.Findings = []securityaudit.Finding{{ID: "auth", Severity: "p2", Body: "Missing authorization"}}
			r.SecurityAudit.Verdict = "rework"
			r.Decision = "changes_requested"
		}, http.StatusOK, "needs_evidence", nil},
		{"security audit missing invocation", func(r *tracker.ReviewChange) { r.SecurityAudit.Run.InvocationID = "" }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"security audit stale base", func(r *tracker.ReviewChange) { r.SecurityAudit.Run.BaseSHA = "other" }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"security audit stale head", func(r *tracker.ReviewChange) { r.SecurityAudit.Run.HeadSHA = "other" }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"security audit old reviewer", func(r *tracker.ReviewChange) { r.SecurityAudit.Run.ReviewerDigest = "other" }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"security audit implementing session", func(r *tracker.ReviewChange) { r.SecurityAudit.Run.ID = 41; r.SecurityAudit.SessionID = 41 }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"security audit backend failure", func(r *tracker.ReviewChange) { r.SecurityAudit.Run.ExitStatus = securityaudit.ExitStatusFailed }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"security audit reconciliation without a provider session", func(_ *tracker.ReviewChange) {}, http.StatusOK, "reviewed", nil},
		{"security audit reconciliation reuses implementing session", func(r *tracker.ReviewChange) { r.SecurityAudit.Run.ID = 41; r.SecurityAudit.SessionID = 41 }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"security audit landing pass", landingAudit, http.StatusOK, "reviewed", nil},
		{"security audit landing after configuration change", landingAudit, http.StatusOK, "reviewed", nil},
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
		{"security audit blocking finding", blockingAudit, http.StatusOK, "needs_evidence", nil},
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
		{"object-backed separate session pass", objectOutput, http.StatusOK, "reviewed", nil},
		{"object-backed another head", func(r *tracker.ReviewChange) { objectOutput(r); r.Validator.HeadSHA = r.Validator.BaseSHA }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"object-backed invalid decision", func(r *tracker.ReviewChange) { objectOutput(r); r.Decision = "invalid" }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"object-backed oversized body", func(r *tracker.ReviewChange) { objectOutput(r); r.Body = strings.Repeat("x", (64<<10)+1) }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"object-backed missing key", func(r *tracker.ReviewChange) { objectOutput(r); r.IdempotencyKey = "" }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"object-backed storage rollback", objectOutput, http.StatusInternalServerError, "needs_evidence", nil},
		{"object-backed storage exhausted", objectOutput, http.StatusTooManyRequests, "needs_evidence", nil},

		{"separate session pass with multiple receipts", multipleReceipts, http.StatusOK, "reviewed", nil},
		{"recovered source inspection retains failed read history", func(r *tracker.ReviewChange) {
			multipleReceipts(r)
			r.Validator.Commands = append(r.Validator.Commands,
				gate.CommandResult{ReceiptID: "timing_missing_source", Command: "git show " + r.Validator.HeadSHA + ":missing.md", HeadSHA: r.Validator.HeadSHA, TreeSHA: strings.Repeat("d", 40), ExitCode: 128},
				gate.CommandResult{ReceiptID: "timing_source", Command: "git show " + r.Validator.HeadSHA + ":README.md", HeadSHA: r.Validator.HeadSHA, TreeSHA: strings.Repeat("d", 40)},
			)
			r.Validator.CriteriaEvidence[0].ReceiptIDs = append(r.Validator.CriteriaEvidence[0].ReceiptIDs, "timing_source")
		}, http.StatusOK, "reviewed", nil},
		{"artifact gate receipt without finalization", multipleReceipts, http.StatusOK, "reviewed", &stageCase{mode: gate.AutomatedReviewOff, gateKind: gate.KindArtifact}},
		{"human gate receipt without finalization", multipleReceipts, http.StatusOK, "needs_evidence", &stageCase{mode: gate.AutomatedReviewOff, gateKind: gate.KindHumanReview}},
		{"failed selected receipt cannot pass", func(r *tracker.ReviewChange) {
			multipleReceipts(r)
			r.Validator.Commands[1].ExitCode = 1
		}, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"separate session pass with human review", func(_ *tracker.ReviewChange) {}, http.StatusOK, "needs_evidence", nil},
		{"implementing session", func(r *tracker.ReviewChange) { r.Validator.SessionID = 41 }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"validator reconciliation without a provider session", func(_ *tracker.ReviewChange) {}, http.StatusOK, "reviewed", nil},
		{"validator reconciliation reuses implementing session", func(r *tracker.ReviewChange) { r.Validator.SessionID = 41 }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"same machine audit session cannot validate acceptance", func(_ *tracker.ReviewChange) {}, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"audit from another machine does not collide", func(_ *tracker.ReviewChange) {}, http.StatusOK, "reviewed", nil},
		{"oversized acceptance body", func(r *tracker.ReviewChange) { r.Body = strings.Repeat("private-payload", 6000) }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"missing session", func(r *tracker.ReviewChange) { r.Validator.SessionID = 0 }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"another head", func(r *tracker.ReviewChange) { r.Validator.HeadSHA = r.Validator.BaseSHA }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"another version", func(r *tracker.ReviewChange) { r.Validator.VersionID = "version_other" }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"human approval from worker", func(r *tracker.ReviewChange) { r.Validator = nil }, http.StatusForbidden, "needs_evidence", nil},
		{"verdict mismatch", func(r *tracker.ReviewChange) { r.Validator.Verdict = "rework" }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"Attribution independent retains rejection", attributedRework, http.StatusOK, "needs_evidence", nil},
		{"Attribution without proof stays unknown", func(r *tracker.ReviewChange) {
			r.Decision, r.Validator.Verdict = "changes_requested", "rework"
			r.Validator.Findings = []gate.Finding{{Severity: "p1", Body: "untouched file failure", Path: "untouched.go", Attribution: "independent"}}
		}, http.StatusOK, "needs_evidence", nil},
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
			var service *Service
			if strings.Contains(test.name, "run-detail backed") {
				service = openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}})
			}
			f := newNativeFixture(t, service, "", "validator")
			var objects *changeObjectFixture
			if strings.HasPrefix(test.name, "object-backed") {
				objects = &changeObjectFixture{}
				f.service.config.content = objects.call
			}
			descriptor := hubTestPolicy()
			if test.name == "missing session" {
				descriptor.Gates.GitHubPullRequest = true
			}
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
			auditCollision := test.name == "same machine audit session cannot validate acceptance" || test.name == "audit from another machine does not collide"
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
			aggregateLanding := test.name == "security audit landing large identities" || test.name == "security audit landing large blocking identities"
			if aggregateLanding {
				cfg := config.Default()
				cfg.Tracker.Kind = config.TrackerHubNative
				cfg.Gate.Validator.Enabled = false
				cfg.Gate.SecurityAudit.Enabled = true
				cfg.Gate.AutomatedReview = gate.AutomatedReviewOptional
				cfg.Review.Human = false
				resolved, err := config.ResolvePolicy(config.Workflow{Config: cfg, Prompt: strings.Repeat("p", 320<<10)})
				if err != nil {
					t.Fatal(err)
				}
				descriptor = resolved
			}
			descriptor = descriptor.WithID()
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			issue := f.create(t, "work")
			largeAudit := strings.HasPrefix(test.name, "security audit large")
			if largeAudit || aggregateLanding {
				fill := "i"
				if aggregateLanding {
					fill = "<"
				}
				body := issue.Body + strings.Repeat(fill, (64<<10)-len(issue.Body))
				response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(issue.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "large-body"}, ExpectedRevision: issue.Revision, Body: &body})
				requireNativeStatus(t, response, http.StatusOK)
			}
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
			var recordedAudit *tracker.SecurityAuditReview
			if audit || auditCollision {
				now := f.service.config.now()
				request.SecurityAudit = &tracker.SecurityAuditReview{SessionID: 42, DiffDigest: policy.Digest([]byte("diff")), Verdict: "pass", Run: securityaudit.Run{ID: 42, InvocationID: "audit-invocation", ProjectID: string(f.project.ID), IssueID: string(issue.WorkItemID), Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(), AuthenticationMode: "api_key", ExitStatus: securityaudit.ExitStatusSuccess, WorkerPID: 1, WorkerPGID: 1, WorkerStartedAt: now, StartedAt: now.Add(-time.Second), CompletedAt: now, OutputBytes: 1, OutputDigest: securityaudit.OutputDigest("result"), Verdict: "pass", Summary: "No findings"}}
				recordedAudit = request.SecurityAudit
				if audit {
					request.Validator = nil
				} else {
					request.SecurityAudit = nil
				}
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
				if test.name == "security audit landing after configuration change" {
					previousID := descriptor.ID
					descriptor.SourceDigest = policy.Digest([]byte("updated configuration"))
					descriptor = descriptor.WithID()
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: previousID, Policy: descriptor}), http.StatusOK)
				}
				lease = claimNativeAttempt(t, f, worker, "machine", "merge-session", issue.WorkItemID, descriptor.ID)
				merge := nativeStartedEvent(lease)
				merge.IdempotencyKey = "merge-start"
				merge.Data.Identity.Role = "merge"
				if test.name == "security audit landing requires merge execution" {
					merge.Data.Identity.Role = "code"
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, merge), http.StatusOK)
				event = merge
				request.LeaseID, request.FencingToken = lease.ID, lease.FencingToken
			}
			if strings.HasPrefix(test.name, "security audit reconciliation") || strings.HasPrefix(test.name, "validator reconciliation") {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
				lease = claimNativeAttempt(t, f, worker, "machine", "reconcile-session", issue.WorkItemID, descriptor.ID)
				reconcile := nativeStartedEvent(lease)
				reconcile.IdempotencyKey = "reconcile-start"
				reconcile.Data.Identity.Role = "code"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, reconcile), http.StatusOK)
				event = reconcile
				request.LeaseID, request.FencingToken = lease.ID, lease.FencingToken
			}
			if strings.Contains(test.name, "visual") {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET require_visual_attachments=1 WHERE id=?", f.project.ID); err != nil {
					t.Fatal(err)
				}
			}

			if test.name == "visual criterion paired upload" {
				_, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,width,height,created_at) SELECT 'att_0123456789abcdef0123456789abcdef',?,?,id,'upload.png','image/png',100,?,1,1,? FROM api_tokens WHERE name='operator-validator'`, f.project.OrganizationID, f.project.ID, policy.Digest([]byte("image")), formatHubTime(time.Now()))
				if err != nil {
					t.Fatal(err)
				}
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
			if counterCollision || auditCollision {
				machine := tracker.MachineID("machine")
				if test.name == "source review from another machine does not collide" || test.name == "audit from another machine does not collide" {
					machine = "other-machine"
				}
				sourceReview := tracker.ChangeReview{SessionMachineID: machine, ID: "review_previous_source", VersionID: version.ID, Decision: "approved", ImplementationReview: &gate.ValidatorResult{VersionID: version.ID, Repository: version.Repository, HeadSHA: version.HeadSHA, BaseSHA: version.BaseSHA, SessionID: 42, Submitted: true, Verdict: "pass", Score: 1}, Actor: tracker.Actor{Kind: "runner", PrincipalID: "previous-worker"}, CreatedAt: f.service.config.now()}
				if auditCollision {
					sourceReview.ImplementationReview = nil
					sourceReview.SecurityAudit = recordedAudit
				}
				raw, err := marshalNative(sourceReview)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO change_evidence (change_id, version_id, kind, record_json) VALUES (?, ?, 'review', ?)", change.ID, version.ID, raw); err != nil {
					t.Fatal(err)
				}
			}
			if largeAudit {
				raw, err := json.Marshal(request)
				if err != nil || len(raw) >= maxAPIRequestBodyBytes {
					t.Fatalf("audit request bytes=%d err=%v", len(raw), err)
				}
				t.Logf("authenticated audit write bytes=%d", len(raw))
			}
			if test.name == "object-backed storage rollback" {
				if _, err := f.service.database.db.ExecContext(t.Context(), `CREATE TRIGGER reject_review_storage BEFORE INSERT ON change_evidence WHEN NEW.kind='review' BEGIN SELECT RAISE(ABORT,'review storage failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			var contentBefore int64
			if objects != nil {
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'`).Scan(&contentBefore); err != nil {
					t.Fatal(err)
				}
			}
			if test.name == "object-backed storage exhausted" {
				f.service.database.hostedOrganization = f.project.OrganizationID
				plans := pilotHostedPlans()
				plans.Plans[0].Allowances["collaboration_bytes"] = contentBefore
				if err := f.service.database.configureHostedPlans(t.Context(), &HostedConfig{Plans: &plans}); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasSuffix(test.name, "attempt changes during preparation") {
				injected := false
				ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(query string, _ []driver.NamedValue) {
					if injected || !strings.Contains(query, "FROM change_requests c JOIN issues i") {
						return
					}
					injected = true
					var raw string
					if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT data_json FROM native_attempts WHERE id=?", version.AttemptID).Scan(&raw); err != nil {
						t.Fatal(err)
					}
					var run tracker.NativeRunData
					if err := json.Unmarshal([]byte(raw), &run); err != nil {
						t.Fatal(err)
					}
					if request.SecurityAudit != nil {
						run.Runtime.Activity.SessionID = request.SecurityAudit.SessionID
					} else {
						run.Runtime.Activity.SessionID = request.Validator.SessionID
					}
					updated, err := json.Marshal(run)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_attempts SET data_json=? WHERE id=?", string(updated), version.AttemptID); err != nil {
						t.Fatal(err)
					}
				}})
				response = httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, newHubAPIRequest(t, http.MethodPost, path+"/versions/"+version.ID+"/reviews", worker, request).WithContext(ctx))
				if !injected {
					t.Fatal("concurrent session change was not injected")
				}
			} else if objects != nil {
				measurement := &changeWriterMeasurement{}
				ctx := context.WithValue(t.Context(), changeWriterMeasurementKey{}, measurement)
				ctx = withRequestTiming(ctx, &requestTiming{writerTrace: func(_ string, _ []driver.NamedValue) {
					measurement.statements++
				}, writerTransaction: func(active bool) { measurement.active = active }})
				response = httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, newHubAPIRequest(t, http.MethodPost, path+"/versions/"+version.ID+"/reviews", worker, request).WithContext(ctx))
				if measurement.statements > 30 || measurement.content != 0 {
					t.Fatalf("review writer statements=%d (bound 30), content operations=%d (want 0)", measurement.statements, measurement.content)
				}
			} else {
				response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", worker, request)
			}
			requireNativeStatus(t, response, test.want)
			if strings.Contains(test.name, "visual") && test.want == http.StatusUnprocessableEntity {
				raw := response.Body.String()
				if !strings.Contains(raw, "requires uploaded screenshots") || strings.Contains(raw, "hidden") || strings.Contains(raw, "figma.com") {
					t.Fatalf("unsafe visual refusal: %s", raw)
				}
			}
			if test.name == "visual criterion paired upload" {
				var bound bool
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM attachment_references WHERE attachment_id='att_0123456789abcdef0123456789abcdef' AND work_item_id=? AND retain_with_work_item=1)", issue.WorkItemID).Scan(&bound); err != nil || !bound {
					t.Fatalf("review reference not bound: %v", err)
				}
			}

			if strings.HasPrefix(test.name, "Attribution") {
				var outcome tracker.ChangeReview
				decodeHubResponse(t, response, &outcome)
				want := "independent"
				if strings.Contains(test.name, "without proof") {
					want = "unknown"
				}
				if outcome.Decision != "changes_requested" || outcome.Validator.Attribution != want || len(outcome.Validator.Findings) != 1 || outcome.Validator.Findings[0].Attribution != want {
					t.Fatalf("lost rejection attribution: %#v", outcome)
				}
			}

			if test.name == "object-backed storage exhausted" {
				request.IdempotencyKey += "-another-output"
				request.Validator.Commands[0].Output += "distinct rejected output"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", worker, request), http.StatusTooManyRequests)
			}
			if objects != nil && test.want != http.StatusOK {
				var charged, registered int64
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'`).Scan(&charged); err != nil {
					t.Fatal(err)
				}
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM change_content_objects`).Scan(&registered); err != nil {
					t.Fatal(err)
				}
				objects.mu.Lock()
				stored := len(objects.objects)
				var storedBytes int64
				for _, data := range objects.objects {
					storedBytes += int64(len(data))
				}
				objects.mu.Unlock()
				expectedObjects := 0
				if test.name == "object-backed storage rollback" {
					expectedObjects = 1
				}
				if charged != contentBefore+storedBytes || registered != int64(expectedObjects) || stored != expectedObjects {
					t.Fatalf("rejected review retained output: charged=%d before=%d registered=%d objects=%d", charged, contentBefore, registered, stored)
				}
			}

			if test.name == "object-backed separate session pass" {
				var stored tracker.ChangeReview
				decodeHubResponse(t, response, &stored)
				if stored.Validator == nil || len(stored.Validator.Commands) != 1 || stored.Validator.Commands[0].Output != request.Validator.Commands[0].Output {
					t.Fatal("review publication lost command output")
				}
				var inline, refs int
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT max(length(record_json)) FROM change_evidence WHERE kind='review'`).Scan(&inline); err != nil {
					t.Fatal(err)
				}
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM change_command_outputs`).Scan(&refs); err != nil {
					t.Fatal(err)
				}
				if inline > 2048 || refs != 1 {
					t.Fatalf("review output remained inline: bytes=%d refs=%d", inline, refs)
				}

				objects.mu.Lock()
				beforeObjects := len(objects.objects)
				beforeWrites := objects.writes
				objects.mu.Unlock()
				var beforeBytes int64
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'`).Scan(&beforeBytes); err != nil {
					t.Fatal(err)
				}
				original := response.Body.String()
				replay := performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", worker, request)
				requireNativeStatus(t, replay, http.StatusOK)
				if replay.Body.String() != original {
					t.Fatal("review replay changed the accepted receipt")
				}
				conflict := request
				conflict.Body = "Different payload under the accepted review key"
				validator := *request.Validator
				validator.Commands = append([]gate.CommandResult(nil), request.Validator.Commands...)
				validator.Commands[0].Output = "different output"
				conflict.Validator = &validator
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", worker, conflict), http.StatusConflict)
				var afterBytes, afterReferences int64
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'`).Scan(&afterBytes); err != nil {
					t.Fatal(err)
				}
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM change_content_objects`).Scan(&afterReferences); err != nil {
					t.Fatal(err)
				}
				objects.mu.Lock()
				afterObjects := len(objects.objects)
				afterWrites := objects.writes
				objects.mu.Unlock()
				if beforeBytes != afterBytes || beforeObjects != afterObjects || beforeWrites != afterWrites || afterReferences != 1 {
					t.Fatal("review replay or key conflict changed stored objects or allowance")
				}
			}

			rejectionField := map[string]string{
				"oversized acceptance body":                              "body",
				"missing session":                                        "validator.session_id",
				"implementing session":                                   "validator.session_id",
				"validator reconciliation reuses implementing session":   "validator.session_id",
				"another head":                                           "validator.head_sha",
				"another version":                                        "validator.version_id",
				"verdict mismatch":                                       "decision",
				"missing evidence cannot pass":                           "validator.criteria_evidence",
				"restated evidence cannot pass":                          "validator.criteria_evidence",
				"failed selected receipt cannot pass":                    "validator.criteria_evidence",
				"required acceptance before source":                      "implementation_review",
				"same machine source session cannot validate acceptance": "validator.session_id",
				"same machine audit session cannot validate acceptance":  "validator.session_id",
			}[test.name]
			if rejectionField != "" {
				var failure hubclient.APIError
				decodeHubResponse(t, response, &failure)
				want := runnerauth.PublicationRejection{Category: "acceptance_validation", Field: rejectionField}
				if failure.Rejection == nil || *failure.Rejection != want {
					t.Fatalf("rejection = %+v, want %+v", failure.Rejection, want)
				}
			}
			auditRejectionField := map[string]string{
				"security audit missing run":                                "run.id",
				"security audit missing session":                            "session_id",
				"security audit foreign issue":                              "run.issue_id",
				"security audit foreign session":                            "session_id",
				"security audit missing invocation":                         "run.invocation_id",
				"security audit stale base":                                 "base_sha",
				"security audit stale head":                                 "head_sha",
				"security audit old reviewer":                               "run.reviewer_version",
				"security audit backend failure":                            "run.exit_status",
				"security audit implementing session":                       "execution_session",
				"security audit reconciliation reuses implementing session": "execution_session",
				"security audit landing stale version":                      "version_id",
				"security audit landing requires merge execution":           "execution_session",
			}[test.name]
			if auditRejectionField != "" {
				failure := hubclient.APIError{Status: response.Code}
				decodeHubResponse(t, response, &failure)
				want := runnerauth.PublicationRejection{Category: "security_audit", Field: auditRejectionField}
				if failure.Rejection == nil || *failure.Rejection != want || failure.Error() != want.DiagnosticError() {
					t.Fatalf("audit rejection = %+v (%v), want %+v", failure.Rejection, failure.Error(), want)
				}
				if terminal := failure.NativeTerminalFailure().Public(); terminal.Rejection == nil || *terminal.Rejection != want || terminal.Stage != "security_audit_publication" || terminal.Attribution != "instance" {
					t.Fatalf("audit terminal failure = %+v", terminal)
				}
			}
			field := map[string]string{"security audit missing invocation": "invocation_id", "security audit missing run": "run.id", "security audit missing session": "session_id", "security audit foreign issue": "issue_id", "security audit foreign project": "project_id", "security audit foreign session": "session_id", "security audit stale base": "base_sha", "security audit stale head": "head_sha"}[test.name]
			if field != "" && !strings.Contains(response.Body.String(), field) {
				t.Fatalf("missing field diagnostic = %s, want %s", response.Body.String(), field)
			}
			if test.name == "missing session" {
				server := httptest.NewServer(f.service.Handler())
				t.Cleanup(server.Close)
				client, err := hubclient.New(hubclient.Config{URL: server.URL, TokenSource: func() string { return worker }, HTTPClient: server.Client()})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native(f.project.OrganizationID, f.project.ID)
				if err != nil {
					t.Fatal(err)
				}
				request.IdempotencyKey = "sdk-rejection"
				request.Body = "private-payload token=private-credential"
				_, err = native.ReviewChange(t.Context(), issue.WorkItemID, change.ID, version.ID, request)
				var failure *hubclient.APIError
				if !errors.As(err, &failure) || failure.Rejection == nil || strings.Contains(err.Error(), "private-payload") || strings.Contains(err.Error(), "private-credential") {
					t.Fatalf("unsafe or missing SDK rejection: %v", err)
				}
				terminal := failure.NativeTerminalFailure()
				terminal.Error, terminal.ObservedAt = err.Error(), f.service.config.now()
				terminal = terminal.Public()
				finished := event
				finished.Type, finished.IdempotencyKey, finished.Data.Sequence = "run.finished", "acceptance-failed", 2
				finished.Data.Outcome = "failed"
				finished.Data.TerminalFailure = &terminal
				publication := &tracker.NativePRPublication{ChangeID: change.ID, VersionID: version.ID, Repository: version.Repository, BaseRef: "develop", Branch: "work", HeadSHA: version.HeadSHA, PolicyID: version.PolicyID, SourceVersion: tracker.NativeChangeReference{ChangeID: change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}, SourceAttemptID: version.AttemptID, External: tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: version.Repository + "/pull/7"}}
				checkpoint := event
				checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Sequence = "run.checkpointed", "published-pr", 2
				checkpoint.Data.Handoff = nativeTestCheckpoint()
				checkpoint.Data.Handoff.HeadSHA = version.HeadSHA
				checkpoint.Data.Handoff.Change = &publication.SourceVersion
				checkpoint.Data.Handoff.ExternalEffect, checkpoint.Data.Handoff.EffectState, checkpoint.Data.Handoff.EffectID = "pr_create", "confirmed", publication.EffectID("pr_create")
				if err := native.AppendEvent(t.Context(), issue.WorkItemID, checkpoint); err != nil {
					t.Fatal(err)
				}
				finished.Data.Sequence = 3
				if _, err := native.PostAttemptDiff(t.Context(), event.Data.AttemptID, tracker.AttemptDiffRequest{Producer: tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: event.Data.AttemptID, LeaseID: lease.ID, FencingToken: lease.FencingToken}, Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 3}, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA}); err != nil {
					t.Fatal(err)
				}
				finished.Data.Finalization = &tracker.NativeFinalization{ObservedAt: terminal.ObservedAt, Settled: true, Changed: true, ChangeID: change.ID, VersionID: version.ID, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, Publication: publication}
				if err := native.AppendEvent(t.Context(), issue.WorkItemID, finished); err != nil {
					t.Fatal(err)
				}
				evidence, err := native.RuntimeEvidence(t.Context(), issue.WorkItemID, event.Data.AttemptID)
				if err != nil {
					t.Fatal(err)
				}
				if evidence.Attempt == nil || evidence.Attempt.TerminalFailure == nil || evidence.Attempt.Finalization == nil {
					t.Fatalf("missing attempt evidence: %+v", evidence)
				}
				receipt, err := operatortool.NativeRuntimeResult(operatortool.WorkAttemptReceipt, operatortool.WorkReadRequest{ProjectID: string(f.project.ID), Reference: string(issue.WorkItemID), NativeAttemptID: event.Data.AttemptID}, evidence)
				if err != nil {
					t.Fatal(err)
				}
				var decoded struct {
					Data tracker.NativeRuntimeEvidence `json:"data"`
				}
				if err := json.Unmarshal(receipt.Content, &decoded); err != nil {
					t.Fatal(err)
				}
				evidence = decoded.Data
				if evidence.Attempt == nil || evidence.Attempt.TerminalFailure == nil || evidence.Attempt.Finalization == nil {
					t.Fatalf("work_attempt_receipt lost evidence: %s", receipt.Content)
				}
				got := evidence.Attempt.TerminalFailure
				source := evidence.Attempt.Finalization
				if got.Stage != "acceptance_publication" || got.Attribution != "instance" || got.Rejection == nil || *got.Rejection != *failure.Rejection || got.Error != failure.Rejection.DiagnosticError() || source.VersionID != version.ID || source.HeadSHA != version.HeadSHA || source.Publication == nil || *source.Publication != *publication || source.Error != "" {
					t.Fatalf("publication identity or acceptance rejection lost: failure=%+v source=%+v", got, source)
				}
				stale := request
				stale.IdempotencyKey, stale.FencingToken = "stale-review", request.FencingToken+1
				refused := performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", worker, stale)
				if refused.Code < 400 || strings.Contains(refused.Body.String(), "acceptance_validation") {
					t.Fatalf("stale authority received acceptance details: %s", refused.Body.String())
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
				request.IdempotencyKey = "expired-review"
				refused = performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", worker, request)
				if refused.Code < 400 || strings.Contains(refused.Body.String(), "acceptance_validation") {
					t.Fatalf("expired authority received acceptance details: %s", refused.Body.String())
				}
				other := newNativeFixture(t, f.service, f.project.OrganizationID, "other-project")
				refused = performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", other.worker(t, "other-worker"), request)
				if refused.Code < 400 || strings.Contains(refused.Body.String(), "acceptance_validation") {
					t.Fatalf("another project received acceptance details: %s", refused.Body.String())
				}
			}
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
			if auditCollision && test.want != http.StatusOK && (len(detail.Reviews) != 1 || detail.Reviews[0].Validator != nil) {
				t.Fatalf("rejected acceptance was persisted: %+v", detail.Reviews)
			}
			if largeAudit {
				server := httptest.NewServer(f.service.Handler())
				t.Cleanup(server.Close)
				client, err := hubclient.New(hubclient.Config{URL: server.URL, TokenSource: func() string { return worker }, HTTPClient: server.Client()})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native(f.project.OrganizationID, f.project.ID)
				if err != nil {
					t.Fatal(err)
				}
				recovery, err := native.Recovery(t.Context(), issue.WorkItemID)
				if err != nil {
					t.Fatalf("authenticated audit blocked SDK recovery: %v", err)
				}
				if recovery.ChangeDetail == nil || !reflect.DeepEqual(recovery.ChangeDetail.Summary, detail.Summary) {
					t.Fatal("audit projection changed summary or review identity")
				}
				if test.name == "security audit large finding identities" {
					if len(recovery.ChangeDetail.Reviews) != 0 || recovery.Read == nil || !slices.Contains(recovery.Read.Omitted, "change_detail.reviews") {
						t.Fatal("audit identities exceeded the review budget without explicit continuation")
					}
					var record []byte
					cursor := ""
					for pages := 0; ; pages++ {
						if pages > 32 {
							t.Fatal("audit identity continuation did not terminate")
						}
						page, err := native.RecoveryEvidence(t.Context(), issue.WorkItemID, cursor)
						if err != nil || page.Section != "reviews" || len(page.Data) > 64<<10 || page.Offset != int64(len(record)) {
							t.Fatalf("audit identity continuation: section=%s err=%v", page.Section, err)
						}
						record = append(record, page.Data...)
						if int64(len(record)) == page.TotalBytes {
							var exact tracker.ChangeReview
							if json.Unmarshal(record, &exact) != nil || !reflect.DeepEqual(exact, detail.Reviews[0]) {
								t.Fatal("review budget lost exact audit finding identities")
							}
							break
						}
						cursor = page.NextCursor
					}
					return
				}
				if len(recovery.ChangeDetail.Reviews) != 1 {
					t.Fatal("authoritative audit is missing")
				}
				projected := recovery.ChangeDetail.Reviews[0]
				original := detail.Reviews[len(detail.Reviews)-1]
				key := securityaudit.Key{ProjectID: string(f.project.ID), Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA}
				evaluation := securityaudit.Evaluate(projected.SecurityAudit.Run, nil, key, "", []string{"p1", "p2"})
				if evaluation.Allowed != (request.Decision == "approved") || evaluation.Reason != securityaudit.Evaluate(original.SecurityAudit.Run, nil, key, "", []string{"p1", "p2"}).Reason || projected.Decision != original.Decision {
					t.Fatal("bounded audit changed security gate semantics")
				}
				raw, err := json.Marshal(recovery)
				if err != nil || len(raw) > 1<<20 {
					t.Fatalf("bounded recovery bytes=%d err=%v", len(raw), err)
				}
				t.Logf("SDK recovery bytes=%d security=%s summary=%s", len(raw), evaluation.Reason, recovery.ChangeDetail.Summary.Status)
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
			current, err := readCurrentChangeDetail(f.service.contentContext(t.Context()), f.service.database.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, detail.Change, f.service.config.now())
			if err != nil || current.Summary.Status != detail.Summary.Status {
				t.Fatalf("current summary differs: %+v, %v", current.Summary, err)
			}
			if test.want == http.StatusOK {
				wantReviews := 1 + stageReviews
				if counterCollision || auditCollision || landing {
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
			if strings.HasPrefix(test.name, "security audit landing large") || test.name == "security audit landing shared identities" {
				observation := event
				observation.Type, observation.IdempotencyKey = "run.observed", "landing-observed"
				observation.Data.Sequence = 2
				observation.Data.Identity.Role = "merge"
				observation.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "merging", Landing: &tracker.NativeLandingReceipt{ChangeID: change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA, BaseSHA: request.SecurityAudit.Run.BaseSHA, SecurityAudit: request.SecurityAudit, GateFailed: true}}
				raw, err := json.Marshal(observation)
				if err != nil || len(raw) >= maxAPIRequestBodyBytes {
					t.Fatalf("landing write bytes=%d err=%v", len(raw), err)
				}
				t.Logf("authenticated landing write bytes=%d", len(raw))
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, observation), http.StatusOK)
				if aggregateLanding {
					for i := range 3 {
						requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("aggregate-review-%d", i)}, Decision: "commented", Body: strings.Repeat("r", 60<<10)}), http.StatusOK)
					}
					for i := range 2 {
						requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("aggregate-comment-%d", i)}, Body: strings.Repeat("c", 60<<10)}), http.StatusOK)
					}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/discussion", f.token, tracker.DiscussChange{Mutation: tracker.Mutation{IdempotencyKey: "aggregate-discussion"}, VersionID: version.ID, Body: strings.Repeat("d", 50<<10)}), http.StatusOK)
					boundedRequest := httptest.NewRequest(http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID)+"?view=recovery", nil)
					boundedRequest.Header.Set("Authorization", "Bearer "+worker)
					boundedRequest.Header.Set("X-Detent-Recovery-Projection", "bounded")
					wire := httptest.NewRecorder()
					f.service.Handler().ServeHTTP(wire, boundedRequest)
					requireNativeStatus(t, wire, http.StatusOK)
					if wire.Body.Len() > 1<<20 {
						t.Fatalf("aggregate landing response exceeds SDK limit: %d", wire.Body.Len())
					}
					t.Logf("authenticated aggregate landing bytes=%d", wire.Body.Len())
				}
				server := httptest.NewServer(f.service.Handler())
				t.Cleanup(server.Close)
				client, err := hubclient.New(hubclient.Config{URL: server.URL, TokenSource: func() string { return worker }, HTTPClient: server.Client()})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native(f.project.OrganizationID, f.project.ID)
				if err != nil {
					t.Fatal(err)
				}
				recovery, err := native.Recovery(t.Context(), issue.WorkItemID)
				if err != nil {
					t.Fatalf("authenticated landing blocked SDK recovery: %v", err)
				}
				raw, err = json.Marshal(recovery)
				if err != nil || len(raw) > 1<<20 {
					t.Fatalf("landing recovery bytes=%d err=%v", len(raw), err)
				}
				if recovery.ChangeDetail == nil || !reflect.DeepEqual(recovery.ChangeDetail.Summary, detail.Summary) {
					t.Fatal("landing projection changed authoritative hold summary")
				}
				copies := []*tracker.NativeLandingReceipt{recovery.ChangeDetail.Versions[0].Landing}
				for _, attempt := range recovery.Attempts {
					if attempt.Runtime != nil && attempt.Runtime.Landing != nil {
						copies = append(copies, attempt.Runtime.Landing)
					}
				}
				if len(copies) != 2 {
					t.Fatalf("landing copies=%d, want version and attempt", len(copies))
				}
				auditBytes, omittedCopies := 0, 0
				for _, receipt := range copies {
					if receipt.SecurityAudit == nil || receipt.SecurityAudit.SessionID != request.SecurityAudit.SessionID || receipt.SecurityAudit.Verdict != request.SecurityAudit.Verdict || receipt.GateFailed != observation.Data.Runtime.Landing.GateFailed || receipt.Refusal != observation.Data.Runtime.Landing.Refusal {
						t.Fatal("landing projection changed audit identity or gate result")
					}
					audit := receipt.SecurityAudit
					encoded, err := json.Marshal(audit)
					if err != nil {
						t.Fatal(err)
					}
					auditBytes += len(encoded)
					if audit.RunOmitted {
						omittedCopies++
						key := securityaudit.Key{ProjectID: string(f.project.ID), Repository: version.Repository, BaseSHA: request.SecurityAudit.Run.BaseSHA, HeadSHA: request.SecurityAudit.Run.HeadSHA}
						if securityaudit.Evaluate(audit.Run, nil, key, "", []string{"p1", "p2"}).Allowed {
							t.Fatal("omitted run became a passing audit")
						}
					}
				}
				if auditBytes > 64<<10 || omittedCopies == 0 {
					t.Fatalf("shared audit budget bytes=%d omissions=%d", auditBytes, omittedCopies)
				}
				if test.name == "security audit landing shared identities" && omittedCopies != 1 {
					t.Fatal("landing copies did not share their byte budget")
				}
				if recovery.Read == nil || !slices.Contains(recovery.Read.Omitted, "attempts.runtime.landing.security_audit.run") {
					t.Fatal("landing omission lacks scoped continuation")
				}
				t.Logf("SDK landing recovery bytes=%d summary=%s", len(raw), recovery.ChangeDetail.Summary.Status)
				seen := map[string]bool{}
				var record []byte
				cursor := ""
				for pages := 0; ; pages++ {
					if pages > 100 {
						t.Fatal("landing continuation did not terminate")
					}
					page, err := native.RecoveryEvidence(t.Context(), issue.WorkItemID, cursor)
					if err != nil {
						t.Fatal(err)
					}
					if page.Section == "complete" {
						break
					}
					if len(page.Data) > 64<<10 || page.Offset != int64(len(record)) {
						t.Fatal("landing fragment limit or offset changed")
					}
					record = append(record, page.Data...)
					if int64(len(record)) == page.TotalBytes {
						var receipt *tracker.NativeLandingReceipt
						switch page.Section {
						case "versions":
							var exact tracker.ChangeVersion
							if err := json.Unmarshal(record, &exact); err != nil {
								t.Fatal(err)
							}
							receipt = exact.Landing
						case "attempts":
							var exact tracker.NativeAttempt
							if err := json.Unmarshal(record, &exact); err != nil {
								t.Fatal(err)
							}
							if exact.Runtime != nil {
								receipt = exact.Runtime.Landing
							}
						}
						if receipt != nil {
							actual, err := json.Marshal(receipt.SecurityAudit)
							if err != nil {
								t.Fatal(err)
							}
							expected, err := json.Marshal(request.SecurityAudit)
							if err != nil {
								t.Fatal(err)
							}
							if string(actual) != string(expected) {
								t.Fatal("continuation lost exact landing audit")
							}
							seen[page.Section] = true
						}
						record = nil
					}
					cursor = page.NextCursor
				}
				if len(seen) != 2 {
					t.Fatal("continuation lost a landing copy")
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
				current, err = readCurrentChangeDetail(f.service.contentContext(t.Context()), f.service.database.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, detail.Change, f.service.config.now())
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
