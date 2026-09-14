package orchestrator

import (
	"context"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// coordinatorSchedulingSource offers the optional coordinator read surface a
// native scheduling source exposes.
type coordinatorSchedulingSource struct {
	SchedulingSource
	reader  runpkg.CoordinatorHubReader
	project string
	asked   []string
}

func (s *coordinatorSchedulingSource) CoordinatorReader(issueID string) runpkg.CoordinatorHubReader {
	s.asked = append(s.asked, issueID)
	return s.reader
}

func (s *coordinatorSchedulingSource) CoordinatorProject(string) string { return s.project }

type stubCoordinatorReader struct{}

func (stubCoordinatorReader) Issue(context.Context, tracker.NativeWorkItemID) (tracker.NativeIssue, error) {
	return tracker.NativeIssue{}, nil
}

func (stubCoordinatorReader) Issues(context.Context, url.Values) (tracker.Page[tracker.NativeIssue], error) {
	return tracker.Page[tracker.NativeIssue]{}, nil
}

func (stubCoordinatorReader) Comments(context.Context, tracker.NativeWorkItemID, string) (tracker.Page[tracker.NativeComment], error) {
	return tracker.Page[tracker.NativeComment]{}, nil
}

func (stubCoordinatorReader) Attempts(context.Context, tracker.NativeWorkItemID, string) (tracker.Page[tracker.NativeAttempt], error) {
	return tracker.Page[tracker.NativeAttempt]{}, nil
}

func TestDispatchModeSelectsCoordinatorByLabel(t *testing.T) {
	t.Parallel()
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress", "Merging"},
		TerminalStates:      []string{"Done"},
	})
	for _, test := range []struct {
		name   string
		labels []string
		state  string
		want   string
	}{
		{name: "exact label", labels: []string{"detent:coordinator"}, state: "In Progress", want: runpkg.RunModeCoordinator},
		{name: "case and space insensitive", labels: []string{"other", " Detent:Coordinator "}, state: "Todo", want: runpkg.RunModeCoordinator},
		{name: "label wins over state", labels: []string{"detent:coordinator"}, state: "Todo", want: runpkg.RunModeCoordinator},
		{name: "near miss", labels: []string{"detent:coordinators"}, state: "In Progress", want: runpkg.RunModeImplement},
		{name: "no label", state: "In Progress", want: runpkg.RunModeImplement},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			state := newState(cfg)
			issue := dispatchTestIssue("issue-coordinator", test.state)
			issue.Labels = test.labels
			o := Orchestrator{cfg: cfg}
			if got := o.dispatchMode(context.Background(), &state, issue); got != test.want {
				t.Fatalf("dispatchMode = %q, want %q", got, test.want)
			}
		})
	}
}

func TestAttachDispatchToolsForCoordinator(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "questions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	source := &coordinatorSchedulingSource{reader: stubCoordinatorReader{}, project: "prj_1"}
	o := &Orchestrator{connector: &questionTracker{Connector: memory.New(memory.Config{})}, workAttempts: db, scheduling: source}
	issue := connector.Issue{ID: "wi_1", Identifier: "prj#12", Labels: []string{"detent:coordinator"}}

	coordinator := RunRequest{Issue: issue, Mode: runpkg.RunModeCoordinator}
	o.attachDispatchTools(&coordinator, runpkg.RunModeCoordinator)
	if len(coordinator.AgentTools) != 0 || coordinator.AgentToolHandler != nil {
		t.Fatalf("coordinator dispatch attached agent tools: %#v", coordinator.AgentTools)
	}
	if coordinator.Coordinator == nil || coordinator.Coordinator.Reader == nil {
		t.Fatalf("coordinator dispatch has no hub reader: %#v", coordinator.Coordinator)
	}
	// The tools report and accept the hub's project id, not the local key.
	if coordinator.Coordinator.ProjectID != "prj_1" {
		t.Fatalf("coordinator project = %q, want the source's hub project", coordinator.Coordinator.ProjectID)
	}
	if len(source.asked) != 1 || source.asked[0] != "wi_1" {
		t.Fatalf("scheduling source was asked for %v, want [wi_1]", source.asked)
	}

	// The same orchestrator still attaches the human-question tool to an
	// ordinary run, so the coordinator branch is what suppresses it.
	implement := RunRequest{Issue: issue, Mode: runpkg.RunModeImplement}
	o.attachDispatchTools(&implement, runpkg.RunModeImplement)
	if len(implement.AgentTools) != 1 || implement.AgentTools[0].Name != "ask_human_question" {
		t.Fatalf("implement dispatch tools = %#v, want the human-question tool", implement.AgentTools)
	}
	if implement.Coordinator != nil {
		t.Fatalf("implement dispatch carries a coordinator reader: %#v", implement.Coordinator)
	}
}

func TestAttachDispatchToolsWithoutCoordinatorSource(t *testing.T) {
	t.Parallel()
	o := &Orchestrator{}
	request := RunRequest{Issue: connector.Issue{ID: "wi_1"}, Mode: runpkg.RunModeCoordinator}
	o.attachDispatchTools(&request, runpkg.RunModeCoordinator)
	if request.Coordinator != nil {
		t.Fatalf("a source without a coordinator reader produced one: %#v", request.Coordinator)
	}
	if len(request.AgentTools) != 0 {
		t.Fatalf("coordinator dispatch attached agent tools: %#v", request.AgentTools)
	}
}
