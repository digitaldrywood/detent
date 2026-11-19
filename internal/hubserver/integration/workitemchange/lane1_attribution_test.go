package workitemchange_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"

	"github.com/digitaldrywood/detent/internal/failureattribution"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/changeattribution"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeFindingAttributionPersistsForTwoItems(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewChangeInOrganization(t, nil)
	second := f.Create(t, "second")
	path := f.Base + "/work-items/" + string(second.WorkItemID) + "/changes"
	response := f.Hub.Request(t, http.MethodPost, path, f.Token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "second-change"}, Title: "Second change"})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	hubfixture.Decode(t, response, &change)
	other := hubfixture.Change{Native: f.Native, Issue: second, Change: change, Path: path + "/" + change.ID, Rules: f.Rules}
	var persisted int
	for index, fixture := range []hubfixture.Change{f, other} {
		version := fixture.Publish(t, fmt.Sprintf("version-%d", index), "")
		diagnostic := fmt.Sprintf("source.go:%d: invalid input", index+1)
		command := fmt.Sprintf("check-input-%d", index)
		observation := failureattribution.Observation{Command: command, HeadSHA: version.BaseSHA, ExitCode: 1, Output: diagnostic, Complete: true}
		receipt := gate.CommandResult{Command: command, HeadSHA: version.HeadSHA, ExitCode: 1, Output: diagnostic, FinishedAt: time.Now(), BaseReproduction: &observation}
		finding := gate.Finding{Body: diagnostic, CheckCommand: command, CheckDiagnostic: diagnostic}
		roles := []string{"validator"}
		if index == 0 {
			roles = append(roles, "implementation_review", "security_audit")
		}
		for _, role := range roles {
			review := tracker.ChangeReview{ID: hubapi.NewID("review"), VersionID: version.ID, Decision: "changes_requested", Body: "Fix the finding", CreatedAt: f.Hub.Now()}
			result := &gate.ValidatorResult{BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, Verdict: "rework", Findings: []gate.Finding{finding}, Commands: []gate.CommandResult{receipt}}
			switch role {
			case "validator":
				review.Validator = result
			case "implementation_review":
				review.ImplementationReview = result
			case "security_audit":
				review.SecurityAudit = &tracker.SecurityAuditReview{Verdict: "rework", Run: securityaudit.Run{BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, Findings: []securityaudit.Finding{{ID: "invalid-input", Severity: "p1", Body: diagnostic, CheckCommand: command, CheckDiagnostic: diagnostic}}, Checks: []securityaudit.CheckComparison{{Head: gate.CommandObservation(receipt), Base: observation}}}}
			}
			ctx := t.Context()
			changeattribution.ReviewSlots(&review)
			raw, err := json.Marshal(review)
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := eventstore.NewContent(f.Hub.Config.RunDetails).PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: f.Project.OrganizationID, ChangeID: fixture.Change.ID, Kind: "review"}, raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Hub.Repository.WriteTenant(ctx, f.Project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.AppendEvidence(ctx, evidence)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		response := f.Hub.Request(t, http.MethodGet, fixture.Path, f.Token, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var detail tracker.ChangeDetail
		hubfixture.Decode(t, response, &detail)
		if len(detail.Reviews) != len(roles) {
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
		}
		if err := f.Hub.Repository.ReadTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.Evidence(t.Context(), fixture.Change.ID, 0, 10)
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
	if persisted != 4 {
		t.Fatalf("persisted reviews=%d, want 4", persisted)
	}
}
