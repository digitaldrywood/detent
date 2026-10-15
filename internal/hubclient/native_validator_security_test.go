package hubclient

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeValidatorReceivesAcceptedSecurityAudit(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		mutate        func(*tracker.ChangeReview)
		want          bool
		blockedBefore bool
		blockedAfter  bool
	}{
		{name: "accepted exact source", want: true},
		{name: "other version", mutate: func(r *tracker.ChangeReview) { r.VersionID = "version_other" }},
		{name: "other base", mutate: func(r *tracker.ChangeReview) { r.SecurityAudit.Run.BaseSHA = strings.Repeat("c", 40) }},
		{name: "other head", mutate: func(r *tracker.ChangeReview) { r.SecurityAudit.Run.HeadSHA = strings.Repeat("c", 40) }},
		{name: "other repository", mutate: func(r *tracker.ChangeReview) { r.SecurityAudit.Run.Repository += "-other" }},
		{name: "other project", mutate: func(r *tracker.ChangeReview) { r.SecurityAudit.Run.ProjectID = "prj_other" }},
		{name: "other issue", mutate: func(r *tracker.ChangeReview) { r.SecurityAudit.Run.IssueID = "wi_other" }},
		{name: "other session", mutate: func(r *tracker.ChangeReview) { r.SecurityAudit.SessionID++ }},
		{name: "other diff", mutate: func(r *tracker.ChangeReview) { r.SecurityAudit.DiffDigest = "other-diff" }},
		{name: "failed execution", mutate: func(r *tracker.ChangeReview) { r.SecurityAudit.Run.ExitStatus = securityaudit.ExitStatusFailed }},
		{name: "requested changes", mutate: func(r *tracker.ChangeReview) { r.Decision = "changes_requested" }},
		{name: "failed audit", mutate: func(r *tracker.ChangeReview) { r.SecurityAudit.Verdict = "rework" }},
		{name: "invalid run verdict", mutate: func(r *tracker.ChangeReview) { r.SecurityAudit.Run.Verdict = "fail" }},
		{name: "missing execution proof", mutate: func(r *tracker.ChangeReview) { r.SecurityAudit.Run.OutputDigest = "" }},
		{name: "old reviewer", mutate: func(r *tracker.ChangeReview) { r.SecurityAudit.Run.ReviewerDigest = "old" }},
		{name: "landing audit", mutate: func(r *tracker.ChangeReview) { r.SecurityAudit.Landing = true }},
		{name: "operator approval", mutate: func(r *tracker.ChangeReview) { r.Actor.Kind = "human" }},
		{name: "earlier block remains authoritative", blockedBefore: true},
		{name: "later block remains authoritative", blockedAfter: true},
		{name: "accepted configured severity policy", want: true, mutate: func(r *tracker.ChangeReview) {
			r.SecurityAudit.Run.Verdict = "fail"
			r.SecurityAudit.Run.Findings = []securityaudit.Finding{{ID: "advisory", Severity: "p3", Body: "Nonblocking finding under the accepted policy"}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			version := tracker.ChangeVersion{ID: "version_example", ChangeVersionInput: tracker.ChangeVersionInput{Repository: "https://github.com/example-org/example-project", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), PolicyID: "policy_example"}, Policy: policy.Descriptor{Gates: policy.Gates{SecurityAudit: true, Validator: true}}}
			diff := tracker.AttemptDiffRequest{BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA}
			raw, err := json.Marshal(diff.Files)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
			run := securityaudit.Run{
				ID: 7, InvocationID: "invocation_example", ProjectID: "prj_example", IssueID: "wi_example",
				Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA,
				ExitStatus: securityaudit.ExitStatusSuccess, Verdict: securityaudit.VerdictPass, Summary: "No findings",
				ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(),
				WorkerPID: 7, WorkerPGID: 7, WorkerStartedAt: now, StartedAt: now, CompletedAt: now.Add(time.Second),
				OutputBytes: 18, OutputDigest: securityaudit.OutputDigest(`{"verdict":"pass"}`),
			}
			review := tracker.ChangeReview{ID: "review_accepted_security", VersionID: version.ID, Decision: "approved", Actor: tracker.Actor{Kind: "runner"}, SecurityAudit: &tracker.SecurityAuditReview{Verdict: "pass", SessionID: 7, DiffDigest: policy.Digest(raw), Run: run}}
			if test.mutate != nil {
				test.mutate(&review)
			}
			detail := tracker.ChangeDetail{Change: tracker.ChangeRequest{ID: "change_example", ProjectID: "prj_example", WorkItemID: "wi_example", CurrentVersion: version.ID}, Versions: []tracker.ChangeVersion{version}, Reviews: []tracker.ChangeReview{review}}
			blocked := review
			blocked.Decision = "changes_requested"
			if test.blockedBefore {
				detail.Reviews = append([]tracker.ChangeReview{blocked}, detail.Reviews...)
			}
			if test.blockedAfter {
				detail.Reviews = append(detail.Reviews, blocked)
			}
			runtime := tracker.NativeRuntimeEvidence{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: "wi_example"}}, Attempt: &tracker.NativeAttempt{NativeRunData: tracker.NativeRunData{AttemptID: "attempt_example", PolicyID: version.PolicyID}}}
			client, err := New(Config{URL: "http://native-hub.test", TokenSource: func() string { return "token" }, HTTPClient: providerHandlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var value any
				switch {
				case strings.HasSuffix(r.URL.Path, "/capabilities"):
					value = nativeCapabilities{Features: []string{tracker.NativeRuntimeEvidenceCapability}}
				case strings.HasSuffix(r.URL.Path, "/runtime"):
					value = runtime
				case strings.HasSuffix(r.URL.Path, "/changes/change_example"):
					page := operatortool.ChangeResult{OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: "wi_example", Detail: &detail, Read: &operatortool.ChangeRead{VersionID: version.ID, Sections: map[string]operatortool.ChangeReadPage{}}}
					for _, section := range []string{"versions", "reviews", "checks", "discussion"} {
						page.Read.Sections[section] = operatortool.ChangeReadPage{Complete: true}
					}
					value = page
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if err := json.NewEncoder(w).Encode(value); err != nil {
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
			execution := &nativeExecution{claim: nativeClaim{source: &NativeConnector{client: native}, lease: tracker.NativeLease{WorkItemID: "wi_example"}}, change: &runner.NativeChange{ChangeID: detail.Change.ID, VersionID: version.ID}, data: tracker.NativeRunData{AttemptID: "attempt_example", PolicyID: version.PolicyID}, lastDiff: &diff}
			input, err := execution.ValidatorVersion(t.Context())
			if err != nil || input.Version == nil {
				t.Fatalf("validator context unavailable: %v", err)
			}
			payload, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(payload), review.ID); got != test.want {
				t.Fatalf("accepted security review in validator transport = %v, want %v", got, test.want)
			}
		})
	}
}
