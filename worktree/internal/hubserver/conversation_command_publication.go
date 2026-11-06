package hubserver

import (
	"github.com/digitaldrywood/detent/internal/tracker"
)

type conversationPreparedRetryKey struct{}

type conversationIssueAllocationKey struct{}

type conversationIssueAllocation struct {
	organization tracker.OrganizationID
	project      tracker.ProjectID
	id           tracker.NativeWorkItemID
	number       int
}
