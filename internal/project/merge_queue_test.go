package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
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
		name   string
		err    error
		paused bool
	}{
		{name: "available"},
		{name: "paused", paused: true},
		{name: "discovery failure retains PR inspection", err: errors.New("rules unavailable")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &mergeQueuePolicyConnector{Connector: memory.New(memory.Config{}), err: tt.err}
			factory := func(workflowconfig.Config) (connector.Connector, error) { return c, nil }
			for range 2 {
				got, err := buildConnector(t.Context(), workflowconfig.Config{}, factory, tt.paused)
				if err != nil || got != c {
					t.Fatalf("connector=%v error=%v", got, err)
				}
			}
			want := 2
			if tt.paused {
				want = 0
			}
			if c.refreshes != want {
				t.Fatalf("refreshes=%d, want load and reload", c.refreshes)
			}
		})
	}
}

func TestUnchangedWorkflowReconciliationDoesNotRefreshMergePolicy(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	if err := os.WriteFile(path, []byte("---\ntracker:\n  kind: memory\n---\nWork.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := globalconfig.Project{Workflow: path}
	workflow, err := LoadWorkflowContext(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := &mergeQueuePolicyConnector{Connector: memory.New(memory.Config{})}
	p := &Project{cfg: cfg, connector: c, workflowSource: WorkflowSourceStatus{Hash: workflow.SourceHash}}
	for range 2 {
		if err := p.reconcileWorkflow(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if c.refreshes != 0 {
		t.Fatalf("refreshes=%d, want no reads for unchanged workflow", c.refreshes)
	}
}
