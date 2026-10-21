package connector_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/testutil/fakehub"
)

func TestDetectCapabilities(t *testing.T) {
	t.Parallel()

	reported := connector.Capabilities{
		DeleteComments: true,
	}

	tests := []struct {
		name string
		conn func(*testing.T) connector.Connector
		want connector.Capabilities
	}{
		{
			name: "nil connector",
			conn: func(*testing.T) connector.Connector {
				return nil
			},
			want: connector.Capabilities{},
		},
		{
			name: "bare connector",
			conn: func(*testing.T) connector.Connector {
				return stubConnector{}
			},
			want: connector.Capabilities{
				UpdateIssueState: true,
				SetAssignee:      true,
				SetField:         true,
				CreateComment:    true,
			},
		},
		{
			name: "probed optional interfaces",
			conn: func(*testing.T) connector.Connector {
				return upsertingPullRequestCommenter{}
			},
			want: connector.Capabilities{
				UpdateIssueState:      true,
				SetAssignee:           true,
				SetField:              true,
				CreateComment:         true,
				CreateWorkItems:       true,
				CommentOnPullRequests: true,
			},
		},
		{
			name: "reported capabilities are authoritative",
			conn: func(*testing.T) connector.Connector {
				return reportingConnector{capabilities: reported}
			},
			want: reported,
		},
		{
			name: "fake Hub connector",
			conn: func(*testing.T) connector.Connector {
				return fakehub.New(fakehub.Config{})
			},
			want: connector.Capabilities{
				CreateWorkItems: true, UpdateComments: true, DeleteComments: true,
				UpdateIssueState:      true,
				SetAssignee:           true,
				SetField:              true,
				CreateComment:         true,
				CloseIssues:           true,
				RemoveFromProject:     true,
				CommentOnPullRequests: true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := connector.DetectCapabilities(tt.conn(t))
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("DetectCapabilities() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

type stubConnector struct{}

func (stubConnector) Name() string {
	return "stub"
}

func (stubConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (stubConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (stubConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (stubConnector) CreateComment(context.Context, string, string) error {
	return connector.ErrNotImplemented
}

func (stubConnector) UpdateIssueState(context.Context, string, string) error {
	return connector.ErrNotImplemented
}

func (stubConnector) SetAssignee(context.Context, string, string) error {
	return connector.ErrNotImplemented
}

func (stubConnector) SetField(context.Context, string, string, string) error {
	return connector.ErrNotImplemented
}

type upsertingPullRequestCommenter struct {
	stubConnector
}

func (upsertingPullRequestCommenter) UpsertIssues(context.Context, []connector.Issue) error {
	return nil
}

func (upsertingPullRequestCommenter) CreatePullRequestComment(context.Context, string, int, string) error {
	return nil
}

type reportingConnector struct {
	upsertingPullRequestCommenter
	capabilities connector.Capabilities
}

func (c reportingConnector) Capabilities() connector.Capabilities {
	return c.capabilities
}
