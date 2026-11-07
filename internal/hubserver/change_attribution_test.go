package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/failureattribution"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
	var outcomes, findings, persisted int
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
			ctx := f.service.contentContext(t.Context())
			reviewAttributionSlots(&review)
			raw, err := json.Marshal(review)
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := eventstore.NewContent(f.service.runDetailStore()).PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: f.project.OrganizationID, ChangeID: fixture.change.ID, Kind: "review"}, raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.config.TenantRepository.WriteTenant(ctx, f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.AppendEvidence(ctx, evidence)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		response := performHubAPIRequest(t, f.service, http.MethodGet, fixture.path, f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var detail tracker.ChangeDetail
		decodeHubResponse(t, response, &detail)
		if len(detail.Reviews) != 3 {
			t.Fatalf("reviews = %d", len(detail.Reviews))
		}
		for _, review := range detail.Reviews {
			var value, findingValue string
			var evidence *failureattribution.Evidence
			switch {
			case review.Validator != nil:
				value, evidence = review.Validator.Attribution, review.Validator.Findings[0].AttributionEvidence
				findingValue = review.Validator.Findings[0].Attribution
			case review.ImplementationReview != nil:
				value, evidence = review.ImplementationReview.Attribution, review.ImplementationReview.Findings[0].AttributionEvidence
				findingValue = review.ImplementationReview.Findings[0].Attribution
			case review.SecurityAudit != nil:
				value, evidence = review.SecurityAudit.Run.Attribution, review.SecurityAudit.Run.Findings[0].AttributionEvidence
				findingValue = review.SecurityAudit.Run.Findings[0].Attribution
			}
			if value != "independent" || findingValue != "independent" || evidence == nil || evidence.BaseSHA != version.BaseSHA || evidence.HeadSHA != version.HeadSHA || evidence.Command != command || evidence.Diagnostic != diagnostic || evidence.HeadOutputDigest == "" || evidence.BaseOutputDigest == "" {
				t.Fatalf("lost attribution: %s %#v", value, evidence)
			}
			outcomes++
			findings++
		}
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.Evidence(t.Context(), fixture.change.ID, 0, 10)
			if err != nil {
				return err
			}
			for _, row := range rows {
				if row.Kind != "review" {
					continue
				}
				if row.VersionID == nil || *row.VersionID != version.ID || row.PayloadObjectKey == "" || row.PayloadSHA256 == "" || row.PayloadBytes <= 0 || row.PayloadContentBytes <= 0 {
					t.Fatalf("invalid persisted review: %#v", row)
				}
				persisted++
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if outcomes != 6 || findings != 6 || persisted != 6 {
		t.Fatalf("persisted attributions: outcomes=%d findings=%d reviews=%d", outcomes, findings, persisted)
	}
}
