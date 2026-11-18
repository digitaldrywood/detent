package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
)

func (c *NativeClient) ProjectDailyTokenSpend(ctx context.Context, _ string, day time.Time) (store.TokenSpend, error) {
	var result store.TokenSpend
	err := c.client.Request(ctx, http.MethodGet, c.base()+"/runner-spend?day="+day.UTC().Format(time.DateOnly), nil, &result)
	return result, err
}

func (c *NativeClient) IssueTokenSpend(ctx context.Context, issue store.IssueIdentity) (store.TokenSpend, error) {
	var result store.TokenSpend
	err := c.client.Request(ctx, http.MethodGet, c.base()+"/runner-spend?issue="+url.QueryEscape(issue.IssueID), nil, &result)
	return result, err
}

func (c *NativeClient) RecentModelTokenQuantiles(ctx context.Context, query store.ModelTokenQuantileQuery) (store.ModelTokenQuantiles, error) {
	var result store.ModelTokenQuantiles
	err := c.client.Request(ctx, http.MethodGet, c.base()+"/runner-model-quantiles?model="+url.QueryEscape(query.Model)+"&limit="+strconv.FormatInt(query.Limit, 10), nil, &result)
	return result, err
}
