package nativechange

import (
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func nativeMutationKey() tracker.Mutation                        { return api.NativeMutationKey() }
func nativeItemPath(id tracker.NativeWorkItemID) (string, error) { return api.NativeItemPath(id) }

func hubErrorCode(err error) string { return api.HubErrorCode(err) }
func claimLost(err error) bool      { return api.ClaimLost(err) }

func issueFromNative(native tracker.NativeIssue) connector.Issue { return api.IssueFromNative(native) }
func nativeMutationError(err error) error                        { return api.NativeMutationError(err) }
