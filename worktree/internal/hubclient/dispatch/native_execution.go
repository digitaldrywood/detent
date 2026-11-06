package dispatch

import (
	"strings"
	"sync"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Scheduler) RunExecution(issueID string) runner.Execution {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.executionFactory == nil {
		return nil
	}
	claim, ok := s.nativeClaims[issueID]
	if !ok {
		if strings.HasPrefix(issueID, "wi_") {
			return s.executionFactory(s, ExecutionClaim{Lease: tracker.NativeLease{WorkItemID: tracker.NativeWorkItemID(issueID)}})
		}
		return nil
	}
	if claim.execution != nil {
		return claim.execution
	}
	if claim.lifecycle == nil {
		claim.lifecycle = &sync.Mutex{}
	}
	claim.execution = s.executionFactory(s, ExecutionClaim{Source: claim.source, Lease: claim.lease, Recovery: claim.recovery, LandingCapacity: claim.landingCapacity, Deadline: claim.deadline})
	s.nativeClaims[issueID] = claim
	return claim.execution
}
