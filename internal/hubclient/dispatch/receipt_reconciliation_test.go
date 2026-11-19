package dispatch

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/stoptest"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestStoppedPublicationReconciliation(t *testing.T) {
	stoptest.PublicationMatrix(t, func(t *testing.T, head string) stoptest.PublicationFixture {
		h := newNativeChangeHub(t, true)
		h.descriptor.Gates.GitHubPullRequest = true
		h.repolicy(t)
		issue := h.createInProgress(t, "Stop after publication")
		h.claim(t, issue.ID)
		e := h.scheduler.RunExecution(issue.ID).(fixtureLifecycle)
		return stoptest.PublicationFixture{PublicationExecution: e, Repository: nativeChangeRepository, Diff: nativeChangeDiff(head, "README.md"), Transport: fixtureClient(h.native.Transport()).requests,
			Release: func() error { return h.scheduler.ReleaseClaim(t.Context(), issue.ID, runner.FinalStateOperatorStopped) },
			Attempt: func() (tracker.NativeAttempt, error) {
				return h.admin.NativeAttempt(t.Context(), tracker.NativeWorkItemID(issue.ID), e.CompletionData().AttemptID)
			},
		}
	})
}
