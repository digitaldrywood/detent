package hubclient

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeSecurityAuditReusesUnchangedVerdict(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		decisions []string
		landing   bool
		oldPrompt bool
		blocked   bool
	}{
		{name: "approved", decisions: []string{"approved"}},
		{name: "blocking audit", decisions: []string{"changes_requested"}, blocked: true},
		{name: "earlier pass does not clear block", decisions: []string{"approved", "changes_requested"}, blocked: true},
		{name: "later pass does not clear block", decisions: []string{"changes_requested", "approved"}, blocked: true},
		{name: "blocking landing audit", decisions: []string{"changes_requested"}, landing: true, blocked: true},
		{name: "prompt update does not clear finding", decisions: []string{"changes_requested"}, oldPrompt: true, blocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version := tracker.ChangeVersion{ID: "version_example", ChangeVersionInput: tracker.ChangeVersionInput{Repository: "https://github.com/example-org/example-project", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), PolicyID: "policy_example"}, Policy: policy.Descriptor{Gates: policy.Gates{SecurityAudit: true}}}
			detail := tracker.ChangeDetail{Change: tracker.ChangeRequest{ID: "change_example", ProjectID: "prj_example", WorkItemID: "wi_example", CurrentVersion: version.ID}, Versions: []tracker.ChangeVersion{version}}
			for _, decision := range tc.decisions {
				audit := &tracker.SecurityAuditReview{Landing: tc.landing, Verdict: "pass", Run: securityaudit.Run{BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest()}}
				if decision == "changes_requested" {
					audit.Verdict = "rework"
					audit.Run.Findings = []securityaudit.Finding{{ID: "auth", Severity: "p1", Body: "Authorization bypass"}}
				}
				if tc.oldPrompt {
					audit.Run.ReviewerDigest = "old"
				}
				detail.Reviews = append(detail.Reviews, tracker.ChangeReview{VersionID: version.ID, Decision: decision, SecurityAudit: audit})
			}
			client, err := New(Config{URL: "http://native-hub.test", TokenSource: func() string { return "token" }, HTTPClient: providerHandlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/changes/change_example") {
					t.Errorf("unchanged terminal audit requested fresh execution evidence: %s", r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				page := operatortool.ChangeResult{OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: "wi_example", Detail: &detail, Read: &operatortool.ChangeRead{VersionID: version.ID, Sections: map[string]operatortool.ChangeReadPage{}}}
				for _, section := range []string{"versions", "reviews", "checks", "discussion"} {
					page.Read.Sections[section] = operatortool.ChangeReadPage{Complete: true}
				}
				if err := json.NewEncoder(w).Encode(page); err != nil {
					t.Error(err)
				}
			}))})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_example", "prj_example")
			if err != nil {
				t.Fatal(err)
			}
			execution := &nativeExecution{claim: nativeClaim{source: &NativeConnector{client: native}, lease: tracker.NativeLease{WorkItemID: "wi_example"}}, change: &runner.NativeChange{ChangeID: detail.Change.ID, VersionID: version.ID, Reviewed: true}, data: tracker.NativeRunData{PolicyID: version.PolicyID}, lastDiff: &tracker.AttemptDiffRequest{BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA}}
			input, err := execution.SecurityAuditVersion(t.Context())
			if err != nil || input.Version != nil {
				t.Fatalf("unchanged terminal audit was scheduled again: %+v, %v", input, err)
			}
			if tc.blocked && (execution.change.Reviewed || execution.change.SecurityAudit == nil || execution.change.SecurityAudit.Verdict != "rework" || len(execution.change.SecurityAudit.Run.Findings) != 1) {
				t.Fatalf("unchanged retry lost blocking audit and Rework routing: %+v", execution.change)
			}
		})
	}
}
