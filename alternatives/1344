package hubclient

import (
	"context"
	"net/http"

	"github.com/digitaldrywood/detent/internal/localboard"
)

func (c *NativeClient) ImportLocalBoard(ctx context.Context, request localboard.Request) (localboard.Result, error) {
	var result localboard.Result
	err := c.client.request(ctx, http.MethodPost, c.base()+"/local-board-import", request, &result)
	return result, err
}
