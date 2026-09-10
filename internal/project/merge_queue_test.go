package project

import (
	"context"
	"errors"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
)

type mergeQueuePolicyConnector struct {
	connector.Connector
	refreshes int
	err       error
}

func (c *mergeQueuePolicyConnector) RefreshMergeQueuePolicy(context.Context) error {
	c.refreshes++
	return c.err
}

func TestBuildConnectorRefreshesMergeQueuePolicy(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		err  error
	}{
		{name: "available"},
		{name: "discovery failure retains PR inspection", err: errors.New("rules unavailable")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &mergeQueuePolicyConnector{Connector: memory.New(memory.Config{}), err: tt.err}
			factory := func(workflowconfig.Config) (connector.Connector, error) { return c, nil }
			for range 2 {
				got, err := buildConnector(t.Context(), workflowconfig.Config{}, factory)
				if err != nil || got != c {
					t.Fatalf("connector=%v error=%v", got, err)
				}
			}
			if c.refreshes != 2 {
				t.Fatalf("refreshes=%d, want load and reload", c.refreshes)
			}
		})
	}
}
