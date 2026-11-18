package fakehubscheduling

import (
	"context"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/testutil/fakehub"
)

func TestCandidateProjectAndWorkflowStates(t *testing.T) {
	t.Parallel()

	source := &Source{Factory: func(id string) (connector.Connector, bool) {
		return fakehub.New(fakehub.Config{Issues: []connector.Issue{
			{ID: id + "-todo", State: "Todo"},
			{ID: id + "-backlog", State: "Backlog"},
		}}), true
	}}
	for _, id := range []string{"alpha", "beta"} {
		for _, test := range []struct {
			states []string
			want   []string
		}{
			{states: []string{"Todo"}, want: []string{id + "-todo"}},
			{states: []string{" todo "}, want: []string{id + "-todo"}},
			{states: []string{"Cancelled"}},
			{want: []string{id + "-backlog", id + "-todo"}},
		} {
			issues, err := source.FetchCandidateIssues(context.Background(), orchestrator.SchedulingRequest{
				ProjectID: id, WorkflowStates: test.states,
			})
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(issues))
			for _, issue := range issues {
				got = append(got, issue.ID)
			}
			slices.Sort(got)
			if !slices.Equal(got, test.want) {
				t.Fatalf("project %s states %v: IDs = %v, want %v", id, test.states, got, test.want)
			}
		}
	}
}
