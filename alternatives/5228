package templates

import (
	"net/url"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func NativeIssuePath(projectID string, item tracker.NativeWorkItemID) string {
	return "/projects/" + url.PathEscape(projectID) + "/issues/" + url.PathEscape(string(item))
}

func ChangePath(projectID string, item tracker.NativeWorkItemID, id string) string {
	return NativeIssuePath(projectID, item) + "/changes/" + url.PathEscape(id)
}
