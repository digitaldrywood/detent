package factory

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/digitaldrywood/symphony-go/internal/connector"
	"github.com/digitaldrywood/symphony-go/internal/connector/memory"
)

func TestNewFromConfigSupportedBackends(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind string
		want string
	}{
		{name: "empty defaults to memory", kind: "", want: "memory"},
		{name: "memory", kind: "memory", want: "memory"},
		{name: "linear", kind: "linear", want: "linear"},
		{name: "github", kind: "github", want: "github"},
		{name: "normalizes whitespace and case", kind: " GitHub ", want: "github"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := NewFromConfig(Config{Kind: tt.kind})
			if err != nil {
				t.Fatalf("NewFromConfig() error = %v", err)
			}
			if got.Name() != tt.want {
				t.Fatalf("Name() = %q, want %q", got.Name(), tt.want)
			}
		})
	}
}

func TestNewFromConfigRejectsNotReadyBackends(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"gitlab", "jira"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			got, err := NewFromConfig(Config{Kind: kind})
			if got != nil {
				t.Fatalf("connector = %T, want nil", got)
			}
			if !errors.Is(err, ErrBackendNotReady) {
				t.Fatalf("error = %v, want ErrBackendNotReady", err)
			}
		})
	}
}

func TestNewFromConfigRejectsUnknownBackend(t *testing.T) {
	t.Parallel()

	got, err := NewFromConfig(Config{Kind: "asana"})
	if got != nil {
		t.Fatalf("connector = %T, want nil", got)
	}
	if !errors.Is(err, ErrUnsupportedBackend) {
		t.Fatalf("error = %v, want ErrUnsupportedBackend", err)
	}
}

func TestFactoryMemoryConnectorUsesConfiguredIssues(t *testing.T) {
	t.Parallel()

	issues := []connector.Issue{{ID: "issue-1", State: "Todo"}}
	var events []memory.Event
	c, err := NewFromConfig(Config{
		Kind: "memory",
		Memory: memory.Config{
			Issues: issues,
			EventSink: func(event memory.Event) {
				events = append(events, event)
			},
		},
	})
	if err != nil {
		t.Fatalf("NewFromConfig() error = %v", err)
	}

	got, err := c.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if !reflect.DeepEqual(got, issues) {
		t.Fatalf("FetchCandidateIssues() = %#v, want %#v", got, issues)
	}

	if err := c.CreateComment(context.Background(), "issue-1", "body"); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}
	if !reflect.DeepEqual(events, []memory.Event{{Kind: memory.EventKindComment, IssueID: "issue-1", Body: "body"}}) {
		t.Fatalf("events = %#v, want comment event", events)
	}
}

func TestFactoryPlaceholderConnectorOperationsAreExplicitlyUnimplemented(t *testing.T) {
	t.Parallel()

	c, err := NewFromConfig(Config{Kind: "github"})
	if err != nil {
		t.Fatalf("NewFromConfig() error = %v", err)
	}

	if _, err := c.FetchCandidateIssues(context.Background()); !errors.Is(err, connector.ErrNotImplemented) {
		t.Fatalf("FetchCandidateIssues() error = %v, want ErrNotImplemented", err)
	}
	if err := c.CreateComment(context.Background(), "issue-1", "body"); !errors.Is(err, connector.ErrNotImplemented) {
		t.Fatalf("CreateComment() error = %v, want ErrNotImplemented", err)
	}
}
