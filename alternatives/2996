package hubserver

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const recoveryTestToken = "fresh-recovery-administrator-token-example"

func TestRecoveryMergeHistoryBoundReportsPossibleOmission(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		page      []eventstore.Event
		index     int
		pageLimit int
		want      bool
	}{
		{name: "later candidate in page", page: []eventstore.Event{{Type: "workflow.transitioned"}, {Type: "workflow.transitioned", FromState: "merging"}}, index: 0, pageLimit: 2, want: true},
		{name: "short final page", page: []eventstore.Event{{Type: "workflow.transitioned"}, {Type: "comment.created"}}, index: 0, pageLimit: 3, want: false},
		{name: "full page may have older candidate", page: []eventstore.Event{{Type: "workflow.transitioned"}, {Type: "comment.created"}}, index: 0, pageLimit: 2, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := recoveryMergeHistoryPageHasMore(test.page, test.index, test.pageLimit); got != test.want {
				t.Fatalf("recoveryMergeHistoryPageHasMore() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestSelfHostedRunnerVersionCompatibility(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		version      string
		major        int
		capabilities []string
		want         int
	}{
		{"older feature set", "older-release", 2, []string{"native_issues", "scoped_collaboration"}, http.StatusOK},
		{"current feature set", "current-release", 2, []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}, http.StatusOK},
		{"unknown required feature", "future-release", 2, []string{"native_issues", "scoped_collaboration", "future_required"}, http.StatusUnprocessableEntity},
		{"unsupported major", "future-release", 99, []string{"native_issues", "scoped_collaboration"}, http.StatusUnprocessableEntity},
		{"missing required feature", "older-release", 2, []string{"native_issues"}, http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newDefaultNativeFixture(t, Config{})
			issue := f.create(t, "work")
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim)
			r.redemption.Version = test.version
			r.enroll(t)
			claim := tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID,
				MachineID: r.binding.MachineID, SessionID: "version-session", TTLSeconds: 90,
				ProtocolMajor: test.major, Capabilities: test.capabilities}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), test.want)
		})
	}
}
