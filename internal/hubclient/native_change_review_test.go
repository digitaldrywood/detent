package hubclient

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// TestIssueFromNativeCarriesChangeReview covers what a hub-native item offers
// a promotion decision. A native item has no pull request of its own, which is
// why every completed native item used to be judged not ready for review and
// left in its active lane (operations.md section 8, the seventh dogfood run);
// the hub's change review surface is what it is judged on instead.
func TestIssueFromNativeCarriesChangeReview(t *testing.T) {
	t.Parallel()

	base := func() tracker.NativeIssue {
		issue := tracker.NativeIssue{Title: "Make the unit test pass", State: "In Progress"}
		issue.WorkItemID = "wi_b6975015ea5642cd9d959b6d006ba9f8"
		issue.ProjectID = "prj_4fe6688481f8493a9d1aae7820fa119d"
		issue.Number = 6
		issue.Revision = 2
		return issue
	}

	tests := []struct {
		name            string
		change          *tracker.NativeIssueChange
		wantReview      *connector.ChangeReview
		wantPullRequest *connector.PullRequest
	}{
		{
			name:   "a resource without the surface is what it always was",
			change: nil,
		},
		{
			name: "a project with no connector reports the change request alone",
			change: &tracker.NativeIssueChange{
				Connector: tracker.NativeChangeConnectorNone,
				ChangeID:  "change_1", State: "open", HeadSHA: "8c16011", Revision: 2,
			},
			wantReview: &connector.ChangeReview{
				PullRequestsAvailable:   false,
				ChangeAtCurrentRevision: true,
				ChangeRevision:          2,
				Revision:                2,
			},
			wantPullRequest: &connector.PullRequest{State: "open", HeadSHA: "8c16011"},
		},
		{
			name: "a change recorded against an older revision does not cover the item",
			change: &tracker.NativeIssueChange{
				Connector: tracker.NativeChangeConnectorNone,
				ChangeID:  "change_1", State: "open", Revision: 1,
			},
			wantReview: &connector.ChangeReview{
				PullRequestsAvailable:   false,
				ChangeAtCurrentRevision: false,
				ChangeRevision:          1,
				Revision:                2,
			},
			wantPullRequest: &connector.PullRequest{State: "open"},
		},
		{
			name: "a project with no connector and no change request has no pull request",
			change: &tracker.NativeIssueChange{
				Connector: tracker.NativeChangeConnectorNone,
			},
			wantReview: &connector.ChangeReview{Revision: 2},
		},
		{
			name: "a mirrored pull request is reported as the pull request",
			change: &tracker.NativeIssueChange{
				Connector: tracker.NativeChangeConnectorGitHub,
				ChangeID:  "change_1", Number: 2434, State: "open", Draft: true,
				URL: "https://github.com/digitaldrywood/detent/pull/2434", HeadSHA: "deadbee", Revision: 2,
			},
			wantReview: &connector.ChangeReview{
				Provider:                tracker.NativeChangeConnectorGitHub,
				PullRequestsAvailable:   true,
				ChangeAtCurrentRevision: true,
				ChangeRevision:          2,
				Revision:                2,
			},
			wantPullRequest: &connector.PullRequest{
				Number: 2434, State: "open", Draft: true,
				URL: "https://github.com/digitaldrywood/detent/pull/2434", HeadSHA: "deadbee",
			},
		},
		{
			name: "a connector whose projection has not seen the pull request keeps the requirement",
			change: &tracker.NativeIssueChange{
				Connector: tracker.NativeChangeConnectorGitHub,
				ChangeID:  "change_1", State: "open", Revision: 2,
			},
			wantReview: &connector.ChangeReview{
				Provider:                tracker.NativeChangeConnectorGitHub,
				PullRequestsAvailable:   true,
				ChangeAtCurrentRevision: true,
				ChangeRevision:          2,
				Revision:                2,
			},
			wantPullRequest: &connector.PullRequest{State: "open"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			native := base()
			native.Change = test.change

			issue := issueFromNative(native)

			if (issue.ChangeReview == nil) != (test.wantReview == nil) {
				t.Fatalf("change review = %#v, want %#v", issue.ChangeReview, test.wantReview)
			}
			if test.wantReview != nil && *issue.ChangeReview != *test.wantReview {
				t.Fatalf("change review = %#v, want %#v", *issue.ChangeReview, *test.wantReview)
			}
			if (issue.PullRequest == nil) != (test.wantPullRequest == nil) {
				t.Fatalf("pull request = %#v, want %#v", issue.PullRequest, test.wantPullRequest)
			}
			if test.wantPullRequest == nil {
				return
			}
			got := *issue.PullRequest
			if got.Number != test.wantPullRequest.Number || got.State != test.wantPullRequest.State ||
				got.Draft != test.wantPullRequest.Draft || got.URL != test.wantPullRequest.URL ||
				got.HeadSHA != test.wantPullRequest.HeadSHA {
				t.Fatalf("pull request = %#v, want %#v", got, *test.wantPullRequest)
			}
			if test.wantPullRequest.Number == 0 {
				if issue.PRNumber != nil {
					t.Fatalf("PRNumber = %d, want none when nothing mirrors the change", *issue.PRNumber)
				}
				return
			}
			if issue.PRNumber == nil || *issue.PRNumber != test.wantPullRequest.Number {
				t.Fatalf("PRNumber = %v, want %d", issue.PRNumber, test.wantPullRequest.Number)
			}
		})
	}
}
