package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

type cleanupDeliveryConnector struct {
	connector.Connector
	associated     connector.Issue
	hydrated       connector.Issue
	associationErr error
	hydrationErr   error
}

func (c *cleanupDeliveryConnector) RevalidatePullRequestAssociation(_ context.Context, issue connector.Issue, includeStatus bool) (connector.Issue, error) {
	if issue.PullRequest != nil {
		panic("cleanup reused snapshot PR")
	}
	if includeStatus {
		panic("cleanup requested PR status")
	}
	if c.associationErr != nil {
		return issue, c.associationErr
	}
	fresh := c.associated
	fresh.PullRequest = c.hydrated.PullRequest
	return fresh, c.hydrationErr
}
func (c *cleanupDeliveryConnector) HydratePullRequest(context.Context, connector.Issue) (connector.Issue, error) {
	panic("cleanup performed duplicate hydration")
}

func TestVerifyCleanupDelivery(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"merged", "closed unmerged", "open", "missing merge time", "unavailable", "degraded", "wrong number", "missing repository", "wrong issue", "unverified association", "association failure", "hydration failure", "missing head"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			now := time.Now()
			number := 42
			issue := connector.Issue{ID: "issue-1", Identifier: "example/repo#1", Closed: true}
			associated := issue
			associated.PRNumber, associated.PRRepository, associated.PRVerifiedAt = &number, "example/repo", now
			hydrated := associated
			hydrated.PullRequest = &connector.PullRequest{Number: number, State: "MERGED", MergedAt: &now, HeadSHA: "original-head"}
			c := &cleanupDeliveryConnector{associated: associated, hydrated: hydrated}
			switch name {
			case "closed unmerged":
				hydrated.PullRequest.State = "CLOSED"
			case "open":
				hydrated.PullRequest.State = "OPEN"
			case "missing merge time":
				hydrated.PullRequest.MergedAt = nil
			case "unavailable":
				hydrated.PullRequest.HydrationUnavailableReason = "unavailable"
			case "degraded":
				hydrated.PullRequest.HydrationDegradedReason = "degraded"
			case "wrong number":
				hydrated.PullRequest.Number++
			case "missing repository":
				c.associated.PRRepository = ""
			case "wrong issue":
				c.associated.ID = "other"
			case "unverified association":
				c.associated.PRVerifiedAt = time.Time{}
			case "association failure":
				c.associationErr = errors.New("offline")
			case "hydration failure":
				c.hydrationErr = errors.New("offline")
			case "missing head":
				hydrated.PullRequest.HeadSHA = ""
			}
			o := &Orchestrator{connector: c}
			head, err := o.verifyCleanupDelivery(t.Context(), issue)
			want := ""
			if name == "merged" {
				want = "original-head"
			}
			if head != want {
				t.Fatalf("head = %q, want %q", head, want)
			}
			wantErr := name == "association failure" || name == "hydration failure"
			if (err != nil) != wantErr {
				t.Fatalf("error = %v, want error %t", err, wantErr)
			}
		})
	}
}

func TestReapWorkspaceRefreshesCleanupDelivery(t *testing.T) {
	for _, reason := range []string{"terminal", "operator_recovery"} {
		for _, closed := range []bool{false, true} {
			t.Run(reason+"/"+strconv.FormatBool(closed), func(t *testing.T) {
				now := time.Now()
				number := 42
				issue := connector.Issue{ID: "issue-1", Identifier: "example/repo#1", Closed: closed, CleanupDeliveredHeadSHA: "stale"}
				associated := issue
				associated.PRNumber, associated.PRRepository, associated.PRVerifiedAt = &number, "example/repo", now
				hydrated := associated
				hydrated.PullRequest = &connector.PullRequest{Number: number, State: "MERGED", MergedAt: &now, HeadSHA: "fresh-head"}
				reaper := &cleanupSweepReaper{}
				o := &Orchestrator{connector: &cleanupDeliveryConnector{associated: associated, hydrated: hydrated}, reaper: reaper, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
				state := newState(Config{})
				if !o.reapWorkspace(t.Context(), &state, issue, reason, now) {
					t.Fatal("cleanup failed")
				}
				want := ""
				if closed {
					want = "fresh-head"
				}
				if len(reaper.issues) != 1 || reaper.issues[0].CleanupDeliveredHeadSHA != want {
					t.Fatalf("cleanup issues = %+v, want delivery %q", reaper.issues, want)
				}
			})
		}
	}
}
