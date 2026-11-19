package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/failureattribution"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient"
	hubapi "github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	serverapi "github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func changeValidatorOperatorScope(t *testing.T, f nativeFixture) hubscope.NativeScope {
	t.Helper()
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("operator credential status=%d: %v", status, err)
	}
	return hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
}

func TestChangeValidatorSessionAuthority(t *testing.T) {
	t.Parallel()
	sourceInput := changeTestInput()
	sourceBundle := changeTestSource(t, &sourceInput)
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
		{"security audit landing incorporated unverified proof", func(r *tracker.ReviewChange) {
			r.SecurityAudit.Landing = true
			r.SecurityAudit.VersionID = r.ExpectedVersionID
		}, http.StatusOK, "reviewed", nil},
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
		{"object-backed referenced validator output", func(r *tracker.ReviewChange) {
			objectOutput(r)
			r.Validator.Commands[0].Output = strings.Repeat("command output\n", 90000)
		}, http.StatusOK, "reviewed", nil},
		{"object-backed separate session pass", objectOutput, http.StatusOK, "reviewed", nil},
		{"object-backed separate session pass with retained history", objectOutput, http.StatusOK, "reviewed", nil},
		{"object-backed another head", func(r *tracker.ReviewChange) { objectOutput(r); r.Validator.HeadSHA = r.Validator.BaseSHA }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"object-backed invalid decision", func(r *tracker.ReviewChange) { objectOutput(r); r.Decision = "invalid" }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"object-backed oversized body", func(r *tracker.ReviewChange) { objectOutput(r); r.Body = strings.Repeat("x", (64<<10)+1) }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"object-backed missing key", func(r *tracker.ReviewChange) { objectOutput(r); r.IdempotencyKey = "" }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"object-backed storage rollback", objectOutput, http.StatusInternalServerError, "needs_evidence", nil},
		{"object-backed storage exhausted", objectOutput, http.StatusTooManyRequests, "needs_evidence", nil},
		{"storage consumed during upload", objectOutput, http.StatusTooManyRequests, "needs_evidence", nil},

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
		{"validator reconciliation for recovered source without a provider session", multipleReceipts, http.StatusOK, "reviewed", nil},
		{"validator positive to zero self review", func(r *tracker.ReviewChange) { r.Validator.SessionID = 41 }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"implementation review positive to zero self review", func(r *tracker.ReviewChange) { implementationReview(r); r.ImplementationReview.SessionID = 41 }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"security audit positive to zero self review", func(r *tracker.ReviewChange) { r.SecurityAudit.SessionID, r.SecurityAudit.Run.ID = 41, 41 }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"validator source zero current implementer self review", func(r *tracker.ReviewChange) { r.Validator.SessionID = 71 }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"implementation review source zero current implementer self review", func(r *tracker.ReviewChange) { implementationReview(r); r.ImplementationReview.SessionID = 71 }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"security audit source zero current implementer self review", func(r *tracker.ReviewChange) { r.SecurityAudit.SessionID, r.SecurityAudit.Run.ID = 71, 71 }, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"validator source zero same attempt self review", func(_ *tracker.ReviewChange) {}, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"implementation review source zero same attempt self review", implementationReview, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"security audit source zero same attempt self review", func(_ *tracker.ReviewChange) {}, http.StatusUnprocessableEntity, "needs_evidence", nil},
		{"implementation review for recovered source without a provider session", implementationReview, http.StatusOK, "needs_evidence", nil},
		{"security audit for recovered source without a provider session", func(_ *tracker.ReviewChange) {}, http.StatusOK, "reviewed", nil},
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
			var objects *changeObjectFixture
			cfg := Config{RunDetails: &memoryRunDetails{}, SecretKeys: secretTestKeys(t, "1", "1")}
			if strings.HasPrefix(test.name, "object-backed") {
				objects = &changeObjectFixture{}
				cfg.Content, cfg.RunDetails = objects.call, objects
			}
			repository, admin := projectRecordPostgresWithAdmin(t, 4, func(config *pgxpool.Config) {
				config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, validatorPGTrace{})
			})
			service := openLocalPostgresHubTestService(t, repository, cfg)
			if test.name == "storage consumed during upload" {
				service.database.detailObjects = repository.DetailStore(service.config.Clock)
				service.config.RunDetails = hubmaintenance.LimitDetails(service.database.detailObjects)
			}
			organization := nativeOrganization{ID: "org_first", Name: "org_first", Slug: "org-first"}
			for _, selected := range []nativeOrganization{{ID: "org_first_second", Name: "org_first_second", Slug: "org-first-second"}, organization} {
				seedPostgresHubAdmin(t, repository, service, selected)
				key := bytes.Repeat([]byte{0x5a}, 32)
				envelope, err := service.config.SecretKeys.Seal(key, accessstore.SecretAAD(selected.ID, "", accessstore.NativeCursorKeyKind))
				if err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteTenant(t.Context(), selected.ID, corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: selected.ID, ID: serverapi.NewID("hub"), CursorKey: key}); err != nil {
						return err
					}
					return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: selected.ID, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: service.config.Clock()}, accessstore.SecretAudit{OrganizationID: selected.ID, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: service.config.Clock()})
				}); err != nil {
					t.Fatal(err)
				}
			}
			service.localOrganization, service.database.hostedOrganization = string(organization.ID), organization.ID
			service.requestMetrics.Organization = organization.ID
			f := newNativeFixture(t, service, organization.ID, "validator")
			var sessionCount int
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM runner_sessions").Scan(&sessionCount); err != nil || sessionCount != 0 {
				t.Fatalf("session identity fixture is not empty: %d, %v", sessionCount, err)
			}
			if _, err := admin.ExecContext(t.Context(), "SELECT setval(pg_get_serial_sequence('runner_sessions','id'),40,true)"); err != nil {
				t.Fatal(err)
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
			recoveredSource := strings.HasSuffix(test.name, "recovered source without a provider session") || strings.Contains(test.name, "source zero current implementer")
			zeroSource := recoveredSource || strings.Contains(test.name, "source zero same attempt")
			if zeroSource {
				event.Data.Identity.Role = "rework"
				event.Data.Runtime.Activity.SessionID = 0
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, event), http.StatusOK)
			mutation := tracker.Mutation{IdempotencyKey: "change", LeaseID: lease.ID, FencingToken: lease.FencingToken}
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/changes"
			response := performHubAPIRequest(t, f.service, http.MethodPost, path, worker, tracker.CreateChange{Mutation: mutation, Title: "Validate immutable change"})
			requireNativeStatus(t, response, http.StatusOK)
			var change tracker.ChangeRequest
			decodeHubResponse(t, response, &change)
			path += "/" + change.ID
			input := sourceInput
			input.PolicyID, input.RunID, input.AttemptID = descriptor.ID, event.Data.RunID, event.Data.AttemptID
			if test.name == "separate session pass with multiple receipts" || test.name == "failed selected receipt cannot pass" || recoveredSource {
				input.Validation = &gate.CommandResult{Command: "make check-land", HeadSHA: input.HeadSHA, TreeSHA: strings.Repeat("d", 40), DurationNS: 1}
			}
			server := httptest.NewServer(f.service.Handler())
			t.Cleanup(server.Close)
			client, err := hubapi.New(hubapi.Config{URL: server.URL, TokenSource: func() string { return worker }, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native(f.project.OrganizationID, f.project.ID)
			if err != nil {
				t.Fatal(err)
			}
			sessionIDs := map[int64]int64{}
			recordSession := func(logical int64, role string) int64 {
				if id, found := sessionIDs[logical]; found {
					return id
				}
				ctx := hubapi.WithMutationAuthority(t.Context(), native.Scope(), lease, nil)
				sessions := hubapi.NewRunnerSessions(native)
				now := f.service.config.Clock()
				id, err := sessions.StartSession(ctx, store.SessionStart{IssueID: string(issue.WorkItemID), StartedAt: now, AgentRole: role, Model: "example-model"})
				if err != nil {
					t.Fatal(err)
				}
				if err := sessions.FinishSession(ctx, id, store.SessionFinish{CompletedAt: now, FinalState: "completed"}); err != nil {
					t.Fatal(err)
				}
				if _, err := sessions.RecordUsageEvent(ctx, store.UsageEvent{IssueID: string(issue.WorkItemID), SessionID: id, StartedAt: now, FinishedAt: now, Outcome: "completed"}); err != nil {
					t.Fatal(err)
				}
				if _, err := sessions.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{IssueID: string(issue.WorkItemID), SessionID: id, StartedAt: now, FinishedAt: now, PhaseType: store.WorkflowPhaseTypeAgentSession, PhaseName: "agent_active", Status: "completed"}); err != nil {
					t.Fatal(err)
				}
				sessionIDs[logical] = id
				return id
			}
			if id := recordSession(41, event.Data.Identity.Role); id != 41 {
				t.Fatalf("source session identity = %d, want 41", id)
			}
			diffSequence := int64(0)
			publishDiff := func(input tracker.ChangeVersionInput) {
				diffSequence++
				if _, err := native.PostAttemptDiff(t.Context(), input.AttemptID, tracker.AttemptDiffRequest{Producer: tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: input.AttemptID, LeaseID: lease.ID, FencingToken: lease.FencingToken}, Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: diffSequence}, BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA}); err != nil {
					t.Fatal(err)
				}
			}
			publishDiff(input)
			mutation.IdempotencyKey = "publish"
			response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions", worker, tracker.PublishChangeVersion{Mutation: mutation, ChangeVersionInput: input, SourceBundle: sourceBundle})
			requireNativeStatus(t, response, http.StatusOK)
			var version tracker.ChangeVersion
			decodeHubResponse(t, response, &version)
			if strings.Contains(test.name, "positive to zero") {
				observation := event
				observation.Type, observation.IdempotencyKey, observation.Data.Sequence = "run.observed", "erase-session", 2
				observation.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", Activity: &workflowmetrics.ActivityProfile{Schema: 1, SessionID: 0}}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, observation), http.StatusOK)
				var frozen eventstore.Version
				if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					frozen, err = r.Version(t.Context(), change.ID, version.ID)
					return err
				}); err != nil || frozen.ImplementingSessionID != 41 {
					t.Fatalf("source session identity was erased: %+v, %v", frozen, err)
				}
			}
			if test.stages != nil && test.stages.gateKind != "" && version.Validation != nil {
				t.Fatal("non-command fixture unexpectedly has a finalization receipt")
			}
			mutation.IdempotencyKey = "validator"
			request := tracker.ReviewChange{Mutation: mutation, ExpectedVersionID: version.ID, Decision: "approved", Validator: &gate.ValidatorResult{SessionID: 42, VersionID: version.ID, Submitted: true, Verdict: "pass", Score: .95, Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, DiffDigest: version.Source.DiffSHA256}}
			request.Validator.CriteriaEvidence = []gate.CriterionEvidence{{Criterion: "Complete the requested work.", Kind: "test", Reference: "change_validator_test.go:TestChangeValidatorSessionAuthority", Behavior: "A separate validator session reviews the immutable version"}}
			acceptanceRequest := request
			var recordedAudit *tracker.SecurityAuditReview
			if audit || auditCollision {
				now := f.service.config.Clock()
				request.SecurityAudit = &tracker.SecurityAuditReview{SessionID: 42, DiffDigest: version.Source.DiffSHA256, Verdict: "pass", Run: securityaudit.Run{ID: 42, InvocationID: "audit-invocation", ProjectID: string(f.project.ID), IssueID: string(issue.WorkItemID), Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(), AuthenticationMode: "api_key", ExitStatus: securityaudit.ExitStatusSuccess, WorkerPID: 1, WorkerPGID: 1, WorkerStartedAt: now, StartedAt: now.Add(-time.Second), CompletedAt: now, OutputBytes: 1, OutputDigest: securityaudit.OutputDigest("result"), Verdict: "pass", Summary: "No findings"}}
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
				sourceAudit.SessionID = recordSession(43, "security_audit")
				sourceAudit.Run.ID = sourceAudit.SessionID
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
			if strings.HasPrefix(test.name, "security audit reconciliation") || strings.HasPrefix(test.name, "validator reconciliation") || recoveredSource {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
				lease = claimNativeAttempt(t, f, worker, "machine", "reconcile-session", issue.WorkItemID, descriptor.ID)
				reconcile := nativeStartedEvent(lease)
				reconcile.IdempotencyKey = "reconcile-start"
				reconcile.Data.Identity.Role = "code"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, reconcile), http.StatusOK)
				if recoveredSource {
					id := recordSession(71, "code")
					observation := reconcile
					observation.Type, observation.IdempotencyKey, observation.Data.Sequence = "run.observed", "recovery-source-session", 2
					observation.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", Activity: &workflowmetrics.ActivityProfile{Schema: 1, SessionID: id}}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, observation), http.StatusOK)
					reconcile = observation
				}
				event = reconcile
				request.LeaseID, request.FencingToken = lease.ID, lease.FencingToken
			}
			if strings.Contains(test.name, "visual") {
				settings := cloudContentSettingsRequest{Mutation: tracker.Mutation{IdempotencyKey: "require-visual"}, ContentChange: projectsettings.ContentChange{RequireVisualAttachments: true}}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/content-settings", testHubAdminToken, settings), http.StatusOK)
			}
			if test.name == "visual criterion paired upload" {
				if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					uploader, err := w.CredentialByHash(t.Context(), apikey.HashToken(f.token))
					if err != nil {
						return err
					}
					return w.CreateAttachment(t.Context(), attachmentstore.Record{OrganizationID: f.project.OrganizationID, Kind: "attachment", Metadata: attachment.Metadata{ID: "att_0123456789abcdef0123456789abcdef", ProjectID: string(f.project.ID), Uploader: uploader.ID, Name: "upload.png", ContentType: "image/png", Size: 100, SHA256: policy.Digest([]byte("image")), Width: 1, Height: 1, CreatedAt: f.service.config.Clock()}})
				}); err != nil {
					t.Fatal(err)
				}
			}

			role := "validator"
			if audit || auditCollision {
				role = "security_audit"
			}
			test.edit(&request)
			stageReviews := 0
			if test.stages != nil {
				if test.stages.retainedAcceptance {
					retained := *acceptanceRequest.Validator
					retained.SessionID = recordSession(44, "validator")
					raw, err := serverapi.MarshalNative(tracker.ChangeReview{ID: "review_retained_acceptance", VersionID: version.ID, Decision: "approved", Validator: &retained, SessionMachineID: lease.MachineID, Actor: tracker.Actor{Kind: "runner", PrincipalID: "previous-worker"}, CreatedAt: f.service.config.Clock()})
					if err != nil {
						t.Fatal(err)
					}
					appendValidatorRetainedReview(t, f, change.ID, version.ID, raw)
					stageReviews++
				}
				for i, verdict := range test.stages.sourceVerdicts {
					source := *acceptanceRequest.Validator
					source.SessionID, source.Verdict = recordSession(int64(50+i), "review"), verdict
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
			recordSession(42, role)
			for _, result := range []*gate.ValidatorResult{request.Validator, request.ImplementationReview} {
				if result != nil {
					if id, found := sessionIDs[result.SessionID]; found {
						result.SessionID = id
					}
				}
			}
			if request.SecurityAudit != nil {
				if id, found := sessionIDs[request.SecurityAudit.SessionID]; found {
					request.SecurityAudit.SessionID = id
				}
				if id, found := sessionIDs[request.SecurityAudit.Run.ID]; found {
					request.SecurityAudit.Run.ID = id
				}
			}
			if counterCollision || auditCollision {
				machine := lease.MachineID
				if test.name == "source review from another machine does not collide" || test.name == "audit from another machine does not collide" {
					machine = "other-machine"
				}
				sourceReview := tracker.ChangeReview{SessionMachineID: machine, ID: "review_previous_source", VersionID: version.ID, Decision: "approved", ImplementationReview: &gate.ValidatorResult{VersionID: version.ID, Repository: version.Repository, HeadSHA: version.HeadSHA, BaseSHA: version.BaseSHA, SessionID: sessionIDs[42], Submitted: true, Verdict: "pass", Score: 1}, Actor: tracker.Actor{Kind: "runner", PrincipalID: "previous-worker"}, CreatedAt: f.service.config.Clock()}
				if auditCollision {
					sourceReview.ImplementationReview = nil
					sourceReview.SecurityAudit = recordedAudit
				}
				raw, err := serverapi.MarshalNative(sourceReview)
				if err != nil {
					t.Fatal(err)
				}
				appendValidatorRetainedReview(t, f, change.ID, version.ID, raw)
			}
			if largeAudit {
				raw, err := json.Marshal(request)
				if err != nil || len(raw) >= serverapi.MaxAPIRequestBodyBytes {
					t.Fatalf("audit request bytes=%d err=%v", len(raw), err)
				}
				t.Logf("authenticated audit write bytes=%d", len(raw))
			}
			if test.name == "object-backed storage rollback" {
				if _, err := admin.ExecContext(t.Context(), `CREATE FUNCTION public.reject_validator_review() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.organization_id=TG_ARGV[0] AND NEW.change_id=TG_ARGV[1] AND NEW.kind='review' THEN RAISE EXCEPTION 'review storage failure'; END IF; RETURN NEW; END $$`); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), "CREATE TRIGGER reject_review_storage BEFORE INSERT ON change_evidence FOR EACH ROW EXECUTE FUNCTION public.reject_validator_review("+pgtransport.QuoteLiteral(string(f.project.OrganizationID))+","+pgtransport.QuoteLiteral(change.ID)+")"); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasSuffix(test.name, "retained history") {
				seedValidatorReviewHistory(t, f, admin, change.ID, issue.WorkItemID, worker)
			}
			var allowance int64
			if test.name == "object-backed storage exhausted" || test.name == "storage consumed during upload" {
				allowance = validatorCollaborationBytes(t, f)
				if test.name == "storage consumed during upload" {
					allowance += 1 << 30
				}
				configureValidatorCollaborationAllowance(t, f, allowance)
			}
			var contentBefore, registeredBefore int64
			var objectsBefore int
			if objects != nil {
				contentBefore = validatorCollaborationBytes(t, f)
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM archive_objects WHERE organization_id=$1", f.project.OrganizationID).Scan(&registeredBefore); err != nil {
					t.Fatal(err)
				}
				objects.mu.Lock()
				objectsBefore = len(objects.objects)
				objects.mu.Unlock()
			}
			if strings.HasSuffix(test.name, "attempt changes during preparation") {
				observation := event
				observation.Type, observation.IdempotencyKey, observation.Data.Sequence = "run.observed", "concurrent-review-session", 2
				observation.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", Activity: &workflowmetrics.ActivityProfile{Schema: 1, SessionID: sessionIDs[42]}}
				injected := false
				var observationResponse *httptest.ResponseRecorder
				probe := &validatorPGProbe{beforeWrite: func() {
					injected = true
					observationResponse = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, observation)
				}}
				ctx := context.WithValue(t.Context(), validatorPGProbeKey{}, probe)
				response = httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, newHubAPIRequest(t, http.MethodPost, path+"/versions/"+version.ID+"/reviews", worker, request).WithContext(ctx))
				if !injected {
					t.Fatal("concurrent session change was not injected")
				}
				requireNativeStatus(t, observationResponse, http.StatusOK)
			} else if test.name == "storage consumed during upload" {
				var before string
				if err := admin.QueryRowContext(t.Context(), "SELECT coalesce(string_agg(object_key,E'\\n'),'') FROM hub_detail_objects WHERE organization_id=$1", f.project.OrganizationID).Scan(&before); err != nil {
					t.Fatal(err)
				}
				referenced := validatorReferencedDetailObjects(t, admin, f)
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM archive_objects WHERE organization_id=$1", f.project.OrganizationID).Scan(&registeredBefore); err != nil {
					t.Fatal(err)
				}
				probe := &validatorPGProbe{measurement: &changeWriterMeasurement{}, beforeReviewWrite: func() {
					if _, err := admin.ExecContext(t.Context(), "UPDATE hosted_usage_counters SET value=$2 WHERE organization_id=$1 AND name='collaboration_bytes'", f.project.OrganizationID, allowance); err != nil {
						t.Error(err)
					}
				}}
				response = httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, newHubAPIRequest(t, http.MethodPost, path+"/versions/"+version.ID+"/reviews", worker, request).WithContext(context.WithValue(t.Context(), validatorPGProbeKey{}, probe)))
				if probe.beforeReviewWrite != nil {
					t.Fatal("allowance consumption was not injected before the review write")
				}
				var reviews, events, receipts, registered int
				var charged int64
				if err := admin.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM change_evidence WHERE organization_id=$1 AND change_id=$2 AND kind='review'),(SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND type='change.review'),(SELECT count(*) FROM native_commands WHERE organization_id=$1 AND command_key=$3),(SELECT count(*) FROM archive_objects WHERE organization_id=$1),(SELECT value FROM hosted_usage_counters WHERE organization_id=$1 AND name='collaboration_bytes')`, f.project.OrganizationID, change.ID, request.IdempotencyKey).Scan(&reviews, &events, &receipts, &registered, &charged); err != nil {
					t.Fatal(err)
				}
				if reviews != 0 || events != 0 || receipts != 0 || int64(registered) != registeredBefore || charged != allowance {
					t.Fatalf("refused review committed reviews=%d events=%d receipts=%d registered=%d charged=%d, want 0/0/0/%d/%d", reviews, events, receipts, registered, charged, registeredBefore, allowance)
				}
				var uploaded string
				if err := admin.QueryRowContext(t.Context(), "SELECT coalesce(string_agg(object_key,E'\\n'),'') FROM hub_detail_objects WHERE organization_id=$1 AND NOT object_key=ANY(string_to_array($2,E'\\n'))", f.project.OrganizationID, before).Scan(&uploaded); err != nil || uploaded == "" {
					t.Fatalf("race did not upload review content before refusal: %v", err)
				}
				if _, err := f.service.database.detailObjects.Reclaim(t.Context(), f.project.OrganizationID, time.Now().Add(attachment.OrphanTTL)); err != nil {
					t.Fatal(err)
				}
				var remaining int
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM hub_detail_objects WHERE organization_id=$1 AND object_key=ANY(string_to_array($2,E'\\n'))", f.project.OrganizationID, uploaded).Scan(&remaining); err != nil || remaining != 0 {
					t.Fatalf("orphan reclamation left %d refused uploads: %v", remaining, err)
				}
				if kept := validatorReferencedDetailObjects(t, admin, f); kept != referenced || kept == 0 {
					t.Fatalf("orphan reclamation changed referenced objects: %d -> %d", referenced, kept)
				}
			} else if test.name == "object-backed referenced validator output" {
				inline := performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", worker, request)
				requireNativeStatus(t, inline, http.StatusRequestEntityTooLarge)
				raw, err := json.Marshal(map[string]any{"organization_id": f.project.OrganizationID, "project_id": f.project.ID, "work_item_id": issue.WorkItemID, "change_id": change.ID, "version_id": version.ID, "review": request})
				if err != nil {
					t.Fatal(err)
				}
				ref, err := objects.StoreDetail(t.Context(), string(f.project.OrganizationID), raw)
				if err != nil {
					t.Fatal(err)
				}
				var uploader string
				if err := admin.QueryRowContext(t.Context(), "SELECT id FROM api_tokens WHERE organization_id=$1 AND token_hash=$2", f.project.OrganizationID, apikey.HashToken(worker)).Scan(&uploader); err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					record := attachmentstore.ArchiveRecord{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, ID: "att_" + ref.SHA256[:32], Uploader: uploader, CreatedAt: f.service.config.Clock(), Reference: ref}
					if _, err := w.PrepareAttachmentArchive(t.Context(), record); err != nil {
						return err
					}
					return w.PublishAttachmentArchive(t.Context(), record, issue.WorkItemID)
				}); err != nil {
					t.Fatal(err)
				}
				wire := map[string]any{"validator_archive": map[string]string{"sha256": ref.SHA256, "content_sha256": policy.Digest(raw)}, "idempotency_key": request.IdempotencyKey, "lease_id": request.LeaseID, "fencing_token": fmt.Sprint(request.FencingToken)}
				wire["decision"] = "approved"
				mixed := performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", worker, wire)
				requireNativeStatus(t, mixed, http.StatusUnprocessableEntity)
				delete(wire, "decision")
				for range 2 {
					response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", worker, wire)
					requireNativeStatus(t, response, http.StatusOK)
					if response.Body.Len() > 4096 {
						t.Fatalf("review acknowledgement bytes=%d", response.Body.Len())
					}
				}
				checkPath := path + "?" + url.Values{"view": {"validator_receipt"}, "idempotency_key": {request.IdempotencyKey}, "version_id": {version.ID}}.Encode()
				check := performHubAPIRequest(t, f.service, http.MethodGet, checkPath, worker, nil)
				requireNativeStatus(t, check, http.StatusOK)
				var ack struct {
					Digest string `json:"request_sha256"`
				}
				decodeHubResponse(t, check, &ack)
				hash, err := serverapi.CommandHash(request)
				if err != nil || ack.Digest != hash {
					t.Fatal("indexed publication receipt did not match original validator command")
				}
				var canonical []byte
				var cursor string
				for {
					page, err := native.RecoveryEvidence(t.Context(), issue.WorkItemID, cursor)
					if err != nil || page.Section != "reviews" || page.VersionID != version.ID || page.Offset != int64(len(canonical)) {
						t.Fatalf("read canonical review at %d: section=%s error=%v", len(canonical), page.Section, err)
					}
					canonical = append(canonical, page.Data...)
					if int64(len(canonical)) == page.TotalBytes {
						if policy.Digest(canonical) != page.SHA256 {
							t.Fatal("canonical review digest mismatch")
						}
						break
					}
					if page.NextCursor == "" || page.NextCursor == cursor {
						t.Fatal("review cursor did not advance")
					}
					cursor = page.NextCursor
				}
				var stored tracker.ChangeReview
				if json.Unmarshal(canonical, &stored) != nil || stored.Validator == nil || len(stored.Validator.Commands) != 1 || stored.Validator.Commands[0].Output != request.Validator.Commands[0].Output {
					t.Fatal("authorized recovery read lost validator output")
				}
				var total int
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM change_evidence WHERE organization_id=$1 AND change_id=$2 AND kind='review'", f.project.OrganizationID, change.ID).Scan(&total); err != nil || total != 1 {
					t.Fatalf("replay stored %d reviews: %v", total, err)
				}
				foreign := newNativeFixture(t, f.service, "org_first_second", "foreign-validator")
				foreignItem := foreign.create(t, "Foreign example item")
				foreignWorker := foreign.worker(t, "foreign-worker")
				foreignPath := foreign.base + "/work-items/" + string(foreignItem.WorkItemID) + "/changes/" + change.ID
				foreignWrite := performHubAPIRequest(t, foreign.service, http.MethodPost, foreignPath+"/versions/"+version.ID+"/reviews", foreignWorker, wire)
				requireNativeStatus(t, foreignWrite, http.StatusNotFound)
				foreignRead := performHubAPIRequest(t, foreign.service, http.MethodGet, foreignPath+strings.TrimPrefix(checkPath, path), foreignWorker, nil)
				requireNativeStatus(t, foreignRead, http.StatusNotFound)
			} else if objects != nil {
				measurement := &changeWriterMeasurement{}
				ctx := context.WithValue(t.Context(), changeWriterMeasurementKey{}, measurement)
				probe := &validatorPGProbe{measurement: measurement}
				ctx = context.WithValue(ctx, validatorPGProbeKey{}, probe)
				response = httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, newHubAPIRequest(t, http.MethodPost, path+"/versions/"+version.ID+"/reviews", worker, request).WithContext(ctx))
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
				if err := admin.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM attachment_item_references WHERE organization_id=$1 AND project_id=$2 AND attachment_id='att_0123456789abcdef0123456789abcdef' AND work_item_id=$3 AND retain_with_work_item)", f.project.OrganizationID, f.project.ID, issue.WorkItemID).Scan(&bound); err != nil || !bound {
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
			if test.name == "object-backed storage rollback" {
				if charged := validatorCollaborationBytes(t, f); charged != contentBefore {
					t.Fatalf("failed review charged bytes=%d, want %d", charged, contentBefore)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", worker, request), http.StatusInternalServerError)
				var reviews, events, receipts int
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM change_evidence WHERE organization_id=$1 AND change_id=$2 AND kind='review'", f.project.OrganizationID, change.ID).Scan(&reviews); err != nil {
					t.Fatal(err)
				}
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND work_item_id=$2 AND type='change.review'", f.project.OrganizationID, issue.WorkItemID).Scan(&events); err != nil {
					t.Fatal(err)
				}
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM native_commands WHERE organization_id=$1 AND operation=$2 AND command_key=$3", f.project.OrganizationID, http.MethodPost+" "+path+"/versions/"+version.ID+"/reviews", request.IdempotencyKey).Scan(&receipts); err != nil {
					t.Fatal(err)
				}
				if reviews != 0 || events != 0 || receipts != 0 {
					t.Fatalf("failed review retry committed reviews=%d events=%d receipts=%d", reviews, events, receipts)
				}
			}
			if objects != nil && test.want != http.StatusOK {
				var charged, registered int64
				charged = validatorCollaborationBytes(t, f)
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM archive_objects WHERE organization_id=$1", f.project.OrganizationID).Scan(&registered); err != nil {
					t.Fatal(err)
				}
				objects.mu.Lock()
				stored := len(objects.objects)
				objects.mu.Unlock()
				expectedObjects := 0
				if test.name == "object-backed storage rollback" {
					expectedObjects = 4
				}
				if charged != contentBefore || registered != registeredBefore || stored-objectsBefore != expectedObjects {
					t.Fatalf("rejected review storage delta: charged=%d registered=%d objects=%d", charged-contentBefore, registered-registeredBefore, stored-objectsBefore)
				}
			}

			if test.name == "object-backed separate session pass" {
				var stored tracker.ChangeReview
				decodeHubResponse(t, response, &stored)
				if stored.Validator == nil || len(stored.Validator.Commands) != 1 || stored.Validator.Commands[0].Output != request.Validator.Commands[0].Output {
					t.Fatal("review publication lost command output")
				}
				var inline, refs int
				if err := admin.QueryRowContext(t.Context(), "SELECT COALESCE(max(octet_length(payload_object_key)+octet_length(payload_sha256)+20),0) FROM change_evidence WHERE organization_id=$1 AND change_id=$2 AND kind='review'", f.project.OrganizationID, change.ID).Scan(&inline); err != nil {
					t.Fatal(err)
				}
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM change_evidence WHERE organization_id=$1 AND change_id=$2 AND version_id=$3 AND kind='review' AND payload_object_key<>''", f.project.OrganizationID, change.ID, version.ID).Scan(&refs); err != nil {
					t.Fatal(err)
				}
				if inline > 2048 || refs != 1 {
					t.Fatalf("review output remained inline: bytes=%d refs=%d", inline, refs)
				}
				var evidence eventstore.Evidence
				var receipt eventstore.Receipt
				var reviewEvent eventstore.Event
				if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					credential, err := r.CredentialByHash(t.Context(), apikey.HashToken(worker))
					if err != nil {
						return err
					}
					receipt, err = r.Receipt(t.Context(), credential.ID, http.MethodPost+" "+path+"/versions/"+version.ID+"/reviews", request.IdempotencyKey)
					if err != nil {
						return err
					}
					rows, err := r.Evidence(t.Context(), change.ID, 0, 2)
					if err != nil {
						return err
					}
					if len(rows) != 1 || rows[0].Kind != "review" {
						return fmt.Errorf("review evidence population = %d, want one review", len(rows))
					}
					evidence = rows[0]
					events, err := r.Events(t.Context(), f.project.ID, issue.WorkItemID, 0, corestore.MaxPageSize)
					if err != nil {
						return err
					}
					if len(events) == corestore.MaxPageSize {
						return errors.New("review fixture history exceeded its complete point read")
					}
					for _, row := range events {
						if row.Type == "change.review" {
							if reviewEvent.ID != "" {
								return errors.New("review event was duplicated")
							}
							reviewEvent = row
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				evidenceRef := eventstore.Reference(evidence.PayloadObjectKey, evidence.PayloadSHA256, evidence.PayloadBytes, evidence.PayloadContentBytes)
				receiptRef := eventstore.Reference(receipt.PayloadObjectKey, receipt.PayloadSHA256, receipt.PayloadBytes, receipt.PayloadContentBytes)
				eventRef := eventstore.Reference(reviewEvent.PayloadObjectKey, reviewEvent.PayloadSHA256, reviewEvent.PayloadBytes, reviewEvent.PayloadContentBytes)
				if evidenceRef != receiptRef || evidenceRef == eventRef || reviewEvent.ID == "" {
					t.Fatalf("review content identity = evidence %+v, receipt %+v, event %+v", evidenceRef, receiptRef, eventRef)
				}
				evidenceRaw, err := f.service.runDetailStore().ReadDetail(t.Context(), string(f.project.OrganizationID), evidenceRef)
				if err != nil {
					t.Fatal(err)
				}
				receiptRaw, err := f.service.runDetailStore().ReadDetail(t.Context(), string(f.project.OrganizationID), receiptRef)
				if err != nil {
					t.Fatal(err)
				}
				eventRaw, err := f.service.runDetailStore().ReadDetail(t.Context(), string(f.project.OrganizationID), eventRef)
				if err != nil {
					t.Fatal(err)
				}
				expectedRaw, err := json.Marshal(stored)
				if err != nil || !bytes.Equal(evidenceRaw, expectedRaw) || !bytes.Equal(receiptRaw, expectedRaw) || bytes.Equal(eventRaw, expectedRaw) {
					t.Fatalf("review/effect immutable bytes differ from the accepted receipt: %v", err)
				}
				outputBytes, err := json.Marshal(request.Validator.Commands[0].Output)
				if err != nil || !bytes.Contains(evidenceRaw, append([]byte(`"output":`), outputBytes...)) || bytes.Contains(eventRaw, outputBytes) {
					t.Fatalf("review output bytes were not confined to its canonical content: %v", err)
				}
				var recordedEvent tracker.CollaborationEvent
				if err := json.Unmarshal(eventRaw, &recordedEvent); err != nil || recordedEvent.Type != "change.review" || recordedEvent.AggregateID != issue.WorkItemID || recordedEvent.Data.Change == nil || recordedEvent.Data.Change.ChangeID != change.ID || recordedEvent.Data.Change.VersionID != version.ID {
					t.Fatalf("review event identity = %+v, %v", recordedEvent, err)
				}
				var charged int64
				if err := admin.QueryRowContext(t.Context(), "SELECT COALESCE(sum(collaboration_bytes),0) FROM archive_objects WHERE organization_id=$1 AND object_key=ANY($2)", f.project.OrganizationID, []string{evidenceRef.Key, eventRef.Key}).Scan(&charged); err != nil || charged != evidenceRef.CompressedSize+eventRef.CompressedSize {
					t.Fatalf("review distinct content charge = %d, want %d: %v", charged, evidenceRef.CompressedSize+eventRef.CompressedSize, err)
				}

				objects.mu.Lock()
				beforeObjects := len(objects.objects)
				beforeWrites := objects.writes
				objects.mu.Unlock()
				var beforeBytes, beforeReferences int64
				beforeBytes = validatorCollaborationBytes(t, f)
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM archive_objects WHERE organization_id=$1", f.project.OrganizationID).Scan(&beforeReferences); err != nil {
					t.Fatal(err)
				}
				if beforeObjects-objectsBefore != 2 || beforeReferences-registeredBefore != 2 || beforeBytes-contentBefore != evidenceRef.CompressedSize+eventRef.CompressedSize {
					t.Fatalf("review distinct content totals: objects=%d registered=%d charged=%d", beforeObjects-objectsBefore, beforeReferences-registeredBefore, beforeBytes-contentBefore)
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
				afterBytes = validatorCollaborationBytes(t, f)
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM archive_objects WHERE organization_id=$1", f.project.OrganizationID).Scan(&afterReferences); err != nil {
					t.Fatal(err)
				}
				objects.mu.Lock()
				afterObjects := len(objects.objects)
				afterWrites := objects.writes
				objects.mu.Unlock()
				if beforeBytes != afterBytes || beforeObjects != afterObjects || beforeWrites != afterWrites || beforeReferences != afterReferences {
					t.Fatal("review replay or key conflict changed stored objects or allowance")
				}
				configureValidatorCollaborationAllowance(t, f, afterBytes)
				objects.mu.Lock()
				exhaustedObjects := len(objects.objects)
				objects.mu.Unlock()
				replay = performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", worker, request)
				requireNativeStatus(t, replay, http.StatusOK)
				objects.mu.Lock()
				replayObjects := len(objects.objects)
				objects.mu.Unlock()
				if replay.Body.String() != original || validatorCollaborationBytes(t, f) != afterBytes || replayObjects != exhaustedObjects {
					t.Fatal("committed review replay at exhausted storage changed its receipt, charges or objects")
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
				terminal.Error, terminal.ObservedAt = err.Error(), f.service.config.Clock()
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
			wantSession := sessionIDs[42]
			if test.stages != nil && test.stages.freshAcceptance {
				fresh := *acceptanceRequest.Validator
				fresh.SessionID = recordSession(43, "validator")
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
				wantSession = sessionIDs[43]
			}
			current, err := f.service.selectedCurrentChangeSummary(t.Context(), changeValidatorOperatorScope(t, f), issue.WorkItemID, detail.Change.ID, f.service.config.Clock())
			if err != nil || current.Status != detail.Summary.Status {
				t.Fatalf("current summary differs: %+v, %v", current, err)
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
				if implementation && wantSession == sessionIDs[42] {
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
				if err != nil || len(raw) >= serverapi.MaxAPIRequestBodyBytes {
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
			if test.name == "security audit landing pass" || test.name == "security audit landing incorporated unverified proof" {
				original := request.SecurityAudit.Run
				incorporated := test.name == "security audit landing incorporated unverified proof"
				mismatches := []string{"base", "audit base", "head", "unrecorded", "modified", "none", "incorporated wrong range", "pass"}
				if incorporated {
					mismatches = append([]string{"source base", "source head", "target head", "target tree", "merge tree", "invalid tree", "missing proof", "missing reviewed head"}, mismatches...)
				}
				for _, mismatch := range mismatches {
					audit := *request.SecurityAudit
					receipt := &tracker.NativeLandingReceipt{Landed: true, SecurityAudit: &audit, VersionID: version.ID, BaseRef: "main", BaseSHA: original.BaseSHA, HeadSHA: version.HeadSHA, MergeSHA: original.HeadSHA}
					if incorporated {
						receipt.MergeSHA = strings.Repeat("e", 40)
						receipt.Incorporation = &tracker.LandingIncorporation{SourceBaseSHA: version.BaseSHA, SourceHeadSHA: version.HeadSHA, TargetHeadSHA: receipt.MergeSHA, TargetTreeSHA: strings.Repeat("a", 40), MergeTreeSHA: strings.Repeat("a", 40)}
					}
					want := http.StatusUnprocessableEntity
					switch mismatch {
					case "incorporated wrong range":
						receipt.BaseSHA = version.BaseSHA
						receipt.MergeSHA = strings.Repeat("e", 40)
						receipt.Incorporation = &tracker.LandingIncorporation{SourceBaseSHA: version.BaseSHA, SourceHeadSHA: version.HeadSHA, TargetHeadSHA: receipt.MergeSHA, TargetTreeSHA: strings.Repeat("a", 40), MergeTreeSHA: strings.Repeat("a", 40)}
						if incorporated {
							audit.Run.BaseSHA = strings.Repeat("d", 40)
						}
					case "audit base":
						audit.Run.BaseSHA = strings.Repeat("f", 40)
					case "source base":
						receipt.Incorporation.SourceBaseSHA = strings.Repeat("f", 40)
					case "source head":
						receipt.Incorporation.SourceHeadSHA = strings.Repeat("f", 40)
					case "target head":
						receipt.Incorporation.TargetHeadSHA = strings.Repeat("f", 40)
					case "target tree":
						receipt.Incorporation.TargetTreeSHA = strings.Repeat("f", 40)
					case "merge tree":
						receipt.Incorporation.MergeTreeSHA = strings.Repeat("f", 40)
					case "invalid tree":
						receipt.Incorporation.TargetTreeSHA, receipt.Incorporation.MergeTreeSHA = "invalid", "invalid"
					case "missing proof":
						receipt.Incorporation = nil
					case "missing reviewed head":
						receipt.HeadSHA = ""
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
						if !incorporated {
							want = http.StatusOK
						}
					}
					land := tracker.LandChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "landing-" + mismatch, LeaseID: lease.ID, FencingToken: lease.FencingToken}, Receipt: receipt, MergeSHA: receipt.MergeSHA, BaseRef: "main", Method: "squash"}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/landing", worker, land), want)
				}
			}
			if test.name == "security audit blocking finding" || test.name == "security audit landing blocking finding" {
				passing := *request.SecurityAudit
				passing.SessionID = recordSession(44, "security_audit")
				passing.Run.ID = passing.SessionID
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
				nextBundle := changeTestSource(t, &input)
				if input.Validation != nil {
					input.Validation.HeadSHA = input.HeadSHA
				}
				publishDiff(input)
				mutation.IdempotencyKey = "publish-next"
				response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions", worker, tracker.PublishChangeVersion{Mutation: mutation, ExpectedVersionID: version.ID, ChangeVersionInput: input, SourceBundle: nextBundle})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &version)
				response = performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
				decodeHubResponse(t, response, &detail)
				current, err = f.service.selectedCurrentChangeSummary(t.Context(), changeValidatorOperatorScope(t, f), issue.WorkItemID, detail.Change.ID, f.service.config.Clock())
				if err != nil || current.Status != "needs_evidence" || current.Status != detail.Summary.Status {
					t.Fatalf("old validator approved new version: %+v, %v", current, err)
				}
				if current.NativeReview != review {
					t.Errorf("old automated review counted as human approval: %+v", current)
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

type claimValidatorStatement struct {
	query string
	args  []any
}

type validatorPGProbeKey struct{}

type validatorPGProbe struct {
	measurement       *changeWriterMeasurement
	writers           map[*pgx.Conn]int
	beforeWrite       func()
	beforeReviewWrite func()
	statements        []claimValidatorStatement
	current           map[*pgx.Conn][]claimValidatorStatement
	began             time.Time
	elapsed           time.Duration
}

type validatorPGTrace struct{}

func (validatorPGTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return traceValidatorStatement(ctx, conn, data.SQL, data.Args)
}

func traceValidatorStatement(ctx context.Context, conn *pgx.Conn, query string, args []any) context.Context {
	probe, ok := ctx.Value(validatorPGProbeKey{}).(*validatorPGProbe)
	if !ok {
		return ctx
	}
	statement := strings.ToLower(strings.TrimSpace(query))
	if strings.HasPrefix(statement, "begin") {
		write := !strings.Contains(statement, "read only")
		if write && probe.beforeWrite != nil {
			inject := probe.beforeWrite
			probe.beforeWrite = nil
			inject()
		}
		if probe.measurement != nil && write {
			if probe.writers == nil {
				probe.writers = make(map[*pgx.Conn]int)
			}
			probe.writers[conn] = 0
			probe.measurement.active = true
			probe.began = time.Now()
		}
		return ctx
	}
	if count, active := probe.writers[conn]; probe.measurement != nil && active && statement != "commit" && statement != "rollback" {
		if count == 1 && probe.beforeReviewWrite != nil && !strings.Contains(statement, "hub_detail_objects") {
			inject := probe.beforeReviewWrite
			probe.beforeReviewWrite = nil
			inject()
		}
		probe.writers[conn] = count + 1
		probe.measurement.statements = max(probe.measurement.statements, count+1)
		if probe.current == nil {
			probe.current = make(map[*pgx.Conn][]claimValidatorStatement)
		}
		probe.current[conn] = append(probe.current[conn], claimValidatorStatement{query, args})
	}
	return ctx
}

func (validatorPGTrace) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, _ pgx.TraceQueryEndData) {
	if probe, ok := ctx.Value(validatorPGProbeKey{}).(*validatorPGProbe); ok && probe.measurement != nil && conn.PgConn().TxStatus() == 'I' {
		if _, active := probe.writers[conn]; active && len(probe.current[conn]) >= len(probe.statements) {
			probe.elapsed, probe.statements = time.Since(probe.began), probe.current[conn]
		}
		delete(probe.current, conn)
		delete(probe.writers, conn)
		probe.measurement.active = len(probe.writers) != 0
	}
}

func (validatorPGTrace) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, query := range data.Batch.QueuedQueries {
		ctx = traceValidatorStatement(ctx, conn, query.SQL, query.Arguments)
	}
	return ctx
}

func (validatorPGTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (validatorPGTrace) TraceBatchEnd(ctx context.Context, conn *pgx.Conn, _ pgx.TraceBatchEndData) {
	if probe, ok := ctx.Value(validatorPGProbeKey{}).(*validatorPGProbe); ok && probe.measurement != nil && conn.PgConn().TxStatus() == 'I' {
		delete(probe.writers, conn)
		probe.measurement.active = len(probe.writers) != 0
	}
}

func validatorCollaborationBytes(t *testing.T, f nativeFixture) int64 {
	t.Helper()
	var used int64
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		used, err = r.CollaborationBytes(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return used
}

func appendValidatorRetainedReview(t *testing.T, f nativeFixture, change, version, raw string) {
	t.Helper()
	row, err := eventstore.NewContent(f.service.runDetailStore()).PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: f.project.OrganizationID, ChangeID: change, VersionID: &version, Kind: "review"}, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.AppendEvidence(t.Context(), row)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func seedValidatorReviewHistory(t *testing.T, f nativeFixture, admin *sql.DB, change string, item tracker.NativeWorkItemID, worker string) {
	t.Helper()
	org := f.project.OrganizationID
	now := f.service.config.Clock().UTC()
	content := eventstore.NewContent(f.service.runDetailStore())
	evidence, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: change, Kind: "discussion", CreatedAt: now}, historyTestJSON(t, tracker.ChangeDiscussion{ID: "cmt_retained", Body: "retained discussion", Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: now}))
	if err != nil {
		t.Fatal(err)
	}
	event, err := content.PrepareEvent(t.Context(), org, historyTestJSON(t, tracker.CollaborationEvent{ID: "evt_retained", OrganizationID: org, ProjectID: f.project.ID, AggregateID: item, Type: "comment.created", SchemaVersion: 1, Actor: tracker.Actor{Kind: "system", PrincipalID: "example"}, RecordedAt: now}))
	if err != nil {
		t.Fatal(err)
	}
	var actor string
	if err := admin.QueryRowContext(t.Context(), "SELECT id FROM api_tokens WHERE organization_id=$1 AND token_hash=$2", org, apikey.HashToken(worker)).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO change_evidence(organization_id,project_id,change_id,sequence,kind,actor_kind,actor_principal_id,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) SELECT $1,$2,$3,c.evidence_sequence+n,'discussion','human','example',$4,$5,$6,$7,$8 FROM change_requests c CROSS JOIN generate_series(1,150)n WHERE c.organization_id=$1 AND c.id=$3`, []any{org, f.project.ID, change, now, evidence.PayloadObjectKey, evidence.PayloadSHA256, evidence.PayloadBytes, evidence.PayloadContentBytes}},
		{`UPDATE change_requests SET evidence_sequence=evidence_sequence+150 WHERE organization_id=$1 AND id=$2`, []any{org, change}},
		{`INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,type,schema_version,actor_kind,actor_principal_id,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,actor_envelope_known) SELECT $1,$2,$3,'evt_retained_'||n,i.event_sequence+n,'comment.created',1,'system','example',$4,$5,$6,$7,$8,true FROM issues i CROSS JOIN generate_series(1,20000)n WHERE i.organization_id=$1 AND i.project_id=$2 AND i.native_id=$3`, []any{org, f.project.ID, item, now, event.PayloadObjectKey, event.PayloadSHA256, event.PayloadBytes, event.PayloadContentBytes}},
		{`UPDATE issues SET event_sequence=event_sequence+20000 WHERE organization_id=$1 AND project_id=$2 AND native_id=$3`, []any{org, f.project.ID, item}},
		{`INSERT INTO native_command_states(organization_id,actor_id,operation,command_key,request_hash,created_at,response_expired) SELECT $1,$2,'example.history',n::text,repeat('a',64),$3,true FROM generate_series(1,10000)n`, []any{org, actor, now}},
	} {
		if _, err := admin.ExecContext(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatalf("seed review history: %v\n%s", err, statement.query)
		}
	}
}

func configureValidatorCollaborationAllowance(t *testing.T, f nativeFixture, allowance int64) {
	t.Helper()
	f.service.database.hostedOrganization = f.project.OrganizationID
	plans := hostedplans.PilotPlans()
	plans.Plans[0].Allowances["collaboration_bytes"] = allowance
	if err := serverapi.FromFeatureError(f.service.database.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&hostedplans.HostedConfig{Plans: &plans}))); err != nil {
		t.Fatal(err)
	}
}

func validatorReferencedDetailObjects(t *testing.T, admin *sql.DB, f nativeFixture) int {
	t.Helper()
	var count int
	if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM hub_detail_objects h WHERE h.organization_id=$1 AND (EXISTS(SELECT 1 FROM archive_objects a WHERE a.organization_id=h.organization_id AND a.object_key=h.object_key) OR EXISTS(SELECT 1 FROM change_versions v WHERE v.organization_id=h.organization_id AND h.object_key IN (v.payload_object_key,v.source_object_key)) OR EXISTS(SELECT 1 FROM collaboration_events e WHERE e.organization_id=h.organization_id AND e.payload_object_key=h.object_key))`, f.project.OrganizationID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
