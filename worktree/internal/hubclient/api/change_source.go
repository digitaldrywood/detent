package api

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (c *NativeClient) ChangeSource(ctx context.Context, item tracker.NativeWorkItemID, change, version string) ([]byte, error) {
	path, err := changePath(item, change, version)
	if err != nil || change == "" || version == "" {
		return nil, errors.New("valid Change Request and version identities are required")
	}
	bundle, _, err := c.client.download(ctx, c.base()+path+"/source", tracker.MaxChangeSourceBytes)
	return bundle, err
}
