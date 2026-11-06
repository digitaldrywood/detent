package dispatch

import (
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func nativeMutationKey() tracker.Mutation                        { return api.NativeMutationKey() }
func nativeItemPath(id tracker.NativeWorkItemID) (string, error) { return api.NativeItemPath(id) }

func hubErrorCode(err error) string         { return api.HubErrorCode(err) }
func claimLost(err error) bool              { return api.ClaimLost(err) }
func cloneTime(value *time.Time) *time.Time { return api.CloneTime(value) }

func queuePriorityName(priority int) string { return api.QueuePriorityName(priority) }
func repositoryPolicyPath(repository string) (string, error) {
	return api.RepositoryPolicyPath(repository)
}

func issueFromNative(native tracker.NativeIssue) connector.Issue { return api.IssueFromNative(native) }
func nativeMutationError(err error) error                        { return api.NativeMutationError(err) }
