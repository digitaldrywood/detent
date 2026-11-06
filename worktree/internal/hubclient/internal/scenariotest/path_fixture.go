package scenariotest

import (
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func changePath(item tracker.NativeWorkItemID, change, version string) (string, error) {
	path, err := nativeItemPath(item)
	if err != nil {
		return "", err
	}
	for _, part := range []struct{ value, prefix string }{{change, "change_"}, {version, "version_"}} {
		if part.value == "" {
			continue
		}
		if !strings.HasPrefix(part.value, part.prefix) || strings.ContainsAny(part.value, "/?#%\\") || part.value == part.prefix {
			return "", errors.New("invalid Change Request or version identity")
		}
	}
	path += "/changes"
	if change != "" {
		path += "/" + change
	}
	if version != "" {
		path += "/versions/" + version
	}
	return path, nil
}
