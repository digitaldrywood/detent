package hubserver

import (
	"database/sql"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/failureattribution"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeFindingAttributionPersistsForTwoItems(t *testing.T) {
	f := newChangeFixture(t, nil)
	second := f.create(t, "second")
	path := f.base + "/work-items/" + string(second.WorkItemID) + "/changes"
	response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "second-change"}, Title: "Second change"})
	requireNativeStatus(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	decodeHubResponse(t, response, &change)
	other := changeFixture{nativeFixture: f.nativeFixture, issue: second, change: change, path: path + "/" + change.ID, rules: f.rules}
	for index, fixture := range []changeFixture{f, other} {
		version := fixture.publish(t, fmt.Sprintf("version-%d", index), "")
		diagnostic := fmt.Sprintf("source.go:%d: invalid input", index+1)
		command := fmt.Sprintf("check-input-%d", index)
		observation := failureattribution.Observation{Command: command, HeadSHA: version.BaseSHA, ExitCode: 1, Output: diagnostic, Complete: true}
		receipt := gate.CommandResult{Command: command, HeadSHA: version.HeadSHA, ExitCode: 1, Output: diagnostic, FinishedAt: time.Now(), BaseReproduction: &observation}
		finding := gate.Finding{Body: diagnostic, CheckCommand: command, CheckDiagnostic: diagnostic}
		for _, role := range []string{"validator", "implementation_review", "security_audit"} {
			review := tracker.ChangeReview{ID: newNativeID("review"), VersionID: version.ID, Decision: "changes_requested", Body: "Fix the finding", CreatedAt: f.service.config.now()}
			result := &gate.ValidatorResult{BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, Verdict: "rework", Findings: []gate.Finding{finding}, Commands: []gate.CommandResult{receipt}}
			switch role {
			case "validator":
				review.Validator = result
			case "implementation_review":
				review.ImplementationReview = result
			case "security_audit":
				review.SecurityAudit = &tracker.SecurityAuditReview{Verdict: "rework", Run: securityaudit.Run{BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, Findings: []securityaudit.Finding{{ID: "invalid-input", Severity: "p1", Body: diagnostic, CheckCommand: command, CheckDiagnostic: diagnostic}}, Checks: []securityaudit.CheckComparison{{Head: gate.CommandObservation(receipt), Base: observation}}}}
			}
			if err := f.service.hubTransact(t.Context(), func(tx *sql.Tx, _ time.Time) error {
				return storeChangeReview(t.Context(), tx, string(f.project.OrganizationID), fixture.change.ID, review)
			}); err != nil {
				t.Fatal(err)
			}
		}
		detail := fixture.detail(t)
		if len(detail.Reviews) != 3 {
			t.Fatalf("reviews = %d", len(detail.Reviews))
		}
		for _, review := range detail.Reviews {
			var value string
			var evidence *failureattribution.Evidence
			switch {
			case review.Validator != nil:
				value, evidence = review.Validator.Attribution, review.Validator.Findings[0].AttributionEvidence
			case review.ImplementationReview != nil:
				value, evidence = review.ImplementationReview.Attribution, review.ImplementationReview.Findings[0].AttributionEvidence
			case review.SecurityAudit != nil:
				value, evidence = review.SecurityAudit.Run.Attribution, review.SecurityAudit.Run.Findings[0].AttributionEvidence
			}
			if value != "independent" || evidence == nil || evidence.BaseSHA != version.BaseSHA || evidence.Command != command || evidence.Diagnostic != diagnostic {
				t.Fatalf("lost attribution: %s %#v", value, evidence)
			}
		}
	}
	var outcomes, findings int
	if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM change_finding_attributions WHERE role IN ('validator','implementation_review','security_audit') AND position=-1 AND attribution='independent'`).Scan(&outcomes); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM change_finding_attributions WHERE role IN ('validator','implementation_review','security_audit') AND position=0 AND attribution='independent' AND base_sha<>'' AND head_sha<>'' AND command<>'' AND head_output_digest<>'' AND base_output_digest<>''`).Scan(&findings); err != nil {
		t.Fatal(err)
	}
	if outcomes != 6 || findings != 6 {
		t.Fatalf("typed attribution rows: outcomes=%d findings=%d", outcomes, findings)
	}
}
