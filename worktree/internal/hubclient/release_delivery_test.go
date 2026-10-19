package hubclient

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReleaseDeliveryLandingTarget(t *testing.T) {
	for _, name := range []string{"ordinary source", "release delivery", "stale association", "stale base", "unreviewed delivery"} {
		t.Run(name, func(t *testing.T) {
			version := tracker.ChangeVersion{ID: "version_release", ChangeID: "change_release", ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Repository: "https://github.com/example-org/example-project", External: &tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: "https://github.com/example-org/example-project/pull/7"}}}
			version.Policy.Gates.SecurityAudit = true
			change := tracker.ChangeRequest{ID: version.ChangeID, OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: "wi_delivery", CurrentVersion: version.ID}
			detail := tracker.ChangeDetail{Change: change, Versions: []tracker.ChangeVersion{version}, Summary: tracker.ChangeSummary{Status: "reviewed"}}
			if name == "unreviewed delivery" {
				detail.Summary.Status = "pending_review"
			}
			client, err := New(Config{URL: "https://hub.example", TokenSource: func() string { return "token" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				var value any
				if strings.HasSuffix(request.URL.Path, "/changes") {
					value = []tracker.ChangeRequest{change}
				} else {
					sections := map[string]operatortool.ChangeReadPage{}
					for _, section := range []string{"versions", "reviews", "checks", "discussion"} {
						sections[section] = operatortool.ChangeReadPage{Complete: true}
					}
					value = operatortool.ChangeResult{OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: "wi_delivery", Detail: &detail, Read: &operatortool.ChangeRead{VersionID: version.ID, Sections: sections}}
				}
				raw, err := json.Marshal(value)
				if err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_example", "prj_example")
			if err != nil {
				t.Fatal(err)
			}
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: "wi_delivery"}}
			if name != "ordinary source" {
				issue.ReleaseDelivery = &tracker.ReleaseDelivery{ReleasePickInput: tracker.ReleasePickInput{SourceMergeSHA: strings.Repeat("d", 40), Repository: version.Repository, PRNumber: 7, HeadRef: "pick/fix", HeadSHA: version.HeadSHA, BaseRef: "release/v1.2", BaseSHA: version.BaseSHA}, ChangeID: change.ID, VersionID: version.ID}
			}
			if name == "stale association" {
				issue.ReleaseDelivery.HeadSHA = strings.Repeat("c", 40)
			}
			if name == "stale base" {
				issue.ReleaseDelivery.BaseSHA = strings.Repeat("c", 40)
			}
			converted := issueFromNative(issue)
			if name == "release delivery" && (converted.PullRequest == nil || converted.PullRequest.BaseRef != "release/v1.2" || converted.PullRequest.HeadSHA != version.HeadSHA || converted.BranchName != "pick/fix" || converted.PRRepository != version.Repository) {
				t.Fatalf("runner issue lost release identity: %+v", converted)
			}
			if name == "ordinary source" && converted.PullRequest != nil {
				t.Fatal("ordinary issue received a release override")
			}
			execution := &nativeExecution{claim: nativeClaim{source: &NativeConnector{client: native}, lease: tracker.NativeLease{WorkItemID: issue.WorkItemID}, recovery: tracker.NativeRecovery{Issue: issue}}}
			target, err := execution.LandingTarget(t.Context())
			if name == "stale association" || name == "stale base" || name == "unreviewed delivery" {
				if !errors.Is(err, runner.ErrLandingNotReviewed) {
					t.Fatalf("unsafe landing target=%+v error=%v", target, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			expected := ""
			if name == "release delivery" {
				expected = "release/v1.2"
			}
			if name == "release delivery" && target.SourceMergeSHA != strings.Repeat("d", 40) {
				t.Fatalf("landing lost source provenance: %+v", target)
			}
			if target.GitHubPullRequest != (name == "release delivery") || !target.SecurityAudit || target.BaseRef != expected || target.HeadSHA != version.HeadSHA || target.External == nil || target.External.ID != "7" {
				t.Fatalf("landing target=%+v", target)
			}
		})
	}
}
