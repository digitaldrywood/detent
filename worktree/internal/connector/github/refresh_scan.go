package github

import (
	"context"
	"fmt"
	"strings"
)

// BeginRefreshScan serializes the refresh loop's GraphQL-heavy fetch and
// hydration stages using the existing credential registry. Other stages and
// other credentials remain independent.
func (c *Connector) BeginRefreshScan(ctx context.Context) (func(), error) {
	token, err := c.client.tokenSource.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve github token: %w", err)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, ErrMissingToken
	}
	state := c.client.bindGraphQLSecondary(token)
	select {
	case state.scans <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-state.scans
			return nil, err
		}
		return func() { <-state.scans }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
