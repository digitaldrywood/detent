package githublocal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/connector/local"
)

func TestConnectorImportPersistsAndDetectsClosedUpstreamDivergence(t *testing.T) {
	t.Parallel()

	server := newGitHubLocalTestServer(t)
	dbPath := filepath.Join(t.TempDir(), "work-items.db")
	cfg := Config{
		GitHub: githubconnector.Config{
			Endpoint: server.URL + "/graphql",
			APIKey:   "ghp_test",
		},
		Local: local.Config{
			Path: dbPath,
		},
		Repository:     "digitaldrywood/detent",
		ActiveStates:   []string{"Todo", "In Progress"},
		TerminalStates: []string{"Done"},
		Now: func() time.Time {
			return time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
		},
	}

	conn, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	imported, err := conn.ImportIssues(context.Background(), []int{779}, "In Progress")
	if err != nil {
		t.Fatalf("ImportIssues() error = %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if len(imported) != 1 {
		t.Fatalf("imported len = %d, want 1", len(imported))
	}
	if imported[0].ID != "github:123:779" {
		t.Fatalf("imported ID = %q, want local surrogate", imported[0].ID)
	}
	if imported[0].Metadata[MetadataDivergence] != DivergenceClosedUpstreamLocalActive {
		t.Fatalf("divergence = %q, want %q", imported[0].Metadata[MetadataDivergence], DivergenceClosedUpstreamLocalActive)
	}
	if imported[0].Closed || imported[0].ClosedReason != "" {
		t.Fatalf("imported closed metadata = (%v, %q), want active local issue", imported[0].Closed, imported[0].ClosedReason)
	}

	restarted, err := New(cfg)
	if err != nil {
		t.Fatalf("restart New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := restarted.Close(); err != nil {
			t.Fatalf("restart Close() error = %v", err)
		}
	})
	issues, err := restarted.FetchIssuesByStates(context.Background(), []string{"In Progress"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues len = %d, want 1", len(issues))
	}
	if issues[0].ID != "github:123:779" || issues[0].State != "In Progress" {
		t.Fatalf("issue identity/state = %q/%q, want local surrogate/In Progress", issues[0].ID, issues[0].State)
	}
	if issues[0].Title != "Closed upstream issue" {
		t.Fatalf("Title = %q, want GitHub title", issues[0].Title)
	}
	if issues[0].Metadata[local.MetadataGitHubRepositoryID] != "123" || issues[0].Metadata[local.MetadataGitHubIssueNumber] != "779" {
		t.Fatalf("GitHub identity metadata = %#v", issues[0].Metadata)
	}
	if issues[0].Metadata[MetadataDivergence] != DivergenceClosedUpstreamLocalActive {
		t.Fatalf("divergence after restart = %q", issues[0].Metadata[MetadataDivergence])
	}
	if issues[0].Closed || issues[0].ClosedReason != "" {
		t.Fatalf("closed metadata after restart = (%v, %q), want active local issue", issues[0].Closed, issues[0].ClosedReason)
	}
}

func TestConnectorLocalAnnotationsStayLocalAndPRLifecycleWritesPassThrough(t *testing.T) {
	t.Parallel()

	server := newGitHubLocalTestServer(t)
	conn, err := New(Config{
		GitHub: githubconnector.Config{
			Endpoint: server.URL + "/graphql",
			APIKey:   "ghp_test",
		},
		Local: local.Config{
			Path: filepath.Join(t.TempDir(), "guard-work-items.db"),
			Issues: []connector.Issue{{
				ID:         "github:123:779",
				Identifier: "digitaldrywood/detent#779",
				Title:      "Local issue",
				State:      "Todo",
				Fields:     map[string]string{},
				Metadata: map[string]string{
					local.MetadataGitHubNodeID:       "I_kwDOtest779",
					local.MetadataGitHubRepositoryID: "123",
					local.MetadataGitHubIssueNumber:  "779",
				},
				AssignedToWorker: true,
			}},
			TerminalStates: []string{"Done"},
		},
		Repository:     "digitaldrywood/detent",
		ActiveStates:   []string{"Todo", "In Progress"},
		TerminalStates: []string{"Done"},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	if err := conn.CreateComment(context.Background(), "github:123:779", "local audit"); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}
	if got := server.writeRequests(); len(got) != 0 {
		t.Fatalf("local annotation wrote to GitHub: %#v", got)
	}

	if err := conn.CreatePullRequestComment(context.Background(), "digitaldrywood/detent", 12, "ship it"); err != nil {
		t.Fatalf("CreatePullRequestComment() error = %v", err)
	}
	if err := conn.MergePullRequest(context.Background(), "digitaldrywood/detent", 12, "head-sha", "merge"); err != nil {
		t.Fatalf("MergePullRequest() error = %v", err)
	}
	got := server.writeRequests()
	want := []githubLocalTestRequest{
		{Method: http.MethodPost, Path: "/repos/digitaldrywood/detent/issues/12/comments"},
		{Method: http.MethodPut, Path: "/repos/digitaldrywood/detent/pulls/12/merge"},
	}
	if len(got) != len(want) {
		t.Fatalf("write request len = %d, want %d: %#v", len(got), len(want), got)
	}
	for index := range want {
		if got[index].Method != want[index].Method || got[index].Path != want[index].Path {
			t.Fatalf("write request[%d] = %#v, want %#v", index, got[index], want[index])
		}
	}
}

func TestConnectorWriteThroughGitHubAuthoritativeMutations(t *testing.T) {
	t.Parallel()

	for _, tt := range writeThroughOperations() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			conn, calls := newRecordingWriteThroughConnector(t, nil, nil)
			if err := tt.run(context.Background(), conn); err != nil {
				t.Fatalf("%s error = %v", tt.name, err)
			}

			want := []string{localLookupCall(), tt.githubCall, tt.localCall}
			if got := calls.snapshot(); !slices.Equal(got, want) {
				t.Fatalf("calls = %#v, want %#v", got, want)
			}
		})
	}
}

func TestConnectorWriteThroughSkipsLocalMutationOnGitHubError(t *testing.T) {
	t.Parallel()

	githubErr := errors.New("github write failed")
	for _, tt := range writeThroughOperations() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			conn, calls := newRecordingWriteThroughConnector(t, map[string]error{tt.githubMethod: githubErr}, nil)
			err := tt.run(context.Background(), conn)
			if !errors.Is(err, githubErr) || err.Error() != githubErr.Error() {
				t.Fatalf("%s error = %v, want unchanged GitHub error", tt.name, err)
			}

			want := []string{localLookupCall(), tt.githubCall}
			if got := calls.snapshot(); !slices.Equal(got, want) {
				t.Fatalf("calls = %#v, want %#v", got, want)
			}
		})
	}
}

func TestConnectorWriteThroughPropagatesBlockedStateError(t *testing.T) {
	t.Parallel()

	blocked := &connector.StateUpdateBlockedError{
		IssueID:      githubWriteThroughIssueID,
		CurrentState: "Done",
		TargetState:  "In Progress",
	}
	conn, calls := newRecordingWriteThroughConnector(t, map[string]error{"UpdateIssueState": blocked}, nil)

	err := conn.UpdateIssueState(context.Background(), localWriteThroughIssueID, "In Progress")
	if !errors.Is(err, connector.ErrStateUpdateBlocked) {
		t.Fatalf("UpdateIssueState() error = %v, want ErrStateUpdateBlocked", err)
	}
	var blockedErr *connector.StateUpdateBlockedError
	if !errors.As(err, &blockedErr) {
		t.Fatalf("UpdateIssueState() error = %T, want StateUpdateBlockedError", err)
	}
	if blockedErr.IssueID != githubWriteThroughIssueID || blockedErr.CurrentState != "Done" || blockedErr.TargetState != "In Progress" {
		t.Fatalf("blocked error = %#v", blockedErr)
	}

	want := []string{
		localLookupCall(),
		call("github.UpdateIssueState", githubWriteThroughIssueID, "In Progress"),
	}
	if got := calls.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("calls = %#v, want %#v", got, want)
	}
}

func TestConnectorWriteThroughFallsBackToLocalWhenGitHubNotImplemented(t *testing.T) {
	t.Parallel()

	for _, tt := range writeThroughOperations() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			conn, calls := newRecordingWriteThroughConnector(t, map[string]error{tt.githubMethod: connector.ErrNotImplemented}, nil)
			if err := tt.run(context.Background(), conn); err != nil {
				t.Fatalf("%s error = %v", tt.name, err)
			}

			want := []string{localLookupCall(), tt.githubCall, tt.localCall}
			if got := calls.snapshot(); !slices.Equal(got, want) {
				t.Fatalf("calls = %#v, want %#v", got, want)
			}
		})
	}
}

func TestConnectorSetFieldFallsBackToLocalWhenGitHubFieldCapabilityMissing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{name: "missing project", err: githubconnector.ErrMissingProject},
		{name: "missing field", err: githubconnector.ErrProjectFieldNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			conn, calls := newRecordingWriteThroughConnector(t, map[string]error{"SetField": tt.err}, nil)
			if err := conn.SetField(context.Background(), localWriteThroughIssueID, "lease", "agent-1"); err != nil {
				t.Fatalf("SetField() error = %v", err)
			}

			want := []string{
				localLookupCall(),
				call("github.SetField", githubWriteThroughIssueID, "lease", "agent-1"),
				call("local.SetField", localWriteThroughIssueID, "lease", "agent-1"),
			}
			if got := calls.snapshot(); !slices.Equal(got, want) {
				t.Fatalf("calls = %#v, want %#v", got, want)
			}
		})
	}
}

func TestConnectorWriteThroughWrapsLocalMirrorFailureAfterGitHubSuccess(t *testing.T) {
	t.Parallel()

	localErr := errors.New("local sqlite write failed")
	conn, calls := newRecordingWriteThroughConnector(t, nil, map[string]error{"UpdateIssueState": localErr})

	err := conn.UpdateIssueState(context.Background(), localWriteThroughIssueID, "In Progress")
	if err == nil {
		t.Fatal("UpdateIssueState() error = nil, want local mirror error")
	}
	if !errors.Is(err, localErr) {
		t.Fatalf("UpdateIssueState() error = %v, want local error in chain", err)
	}
	if !strings.Contains(err.Error(), "github state applied; local mirror update failed") {
		t.Fatalf("UpdateIssueState() error = %q, want GitHub/local mirror context", err.Error())
	}

	want := []string{
		localLookupCall(),
		call("github.UpdateIssueState", githubWriteThroughIssueID, "In Progress"),
		call("local.UpdateIssueState", localWriteThroughIssueID, "In Progress"),
	}
	if got := calls.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("calls = %#v, want %#v", got, want)
	}
}

func TestConnectorFetchIssueStatesByIdentifiersReturnsLocalOnlyIssues(t *testing.T) {
	t.Parallel()

	server := newGitHubLocalTestServer(t)
	conn, err := New(Config{
		GitHub: githubconnector.Config{
			Endpoint: server.URL + "/graphql",
			APIKey:   "ghp_test",
		},
		Local: local.Config{
			Path: filepath.Join(t.TempDir(), "local-only-work-items.db"),
			Issues: []connector.Issue{{
				ID:         "external-123",
				Identifier: "external-123",
				Title:      "Runtime item",
				State:      "Todo",
				Fields:     map[string]string{},
			}},
		},
		Repository:   "digitaldrywood/detent",
		ActiveStates: []string{"Todo", "In Progress"},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	issues, err := conn.FetchIssueStatesByIdentifiers(context.Background(), []string{"external-123"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIdentifiers() error = %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues len = %d, want 1: %#v", len(issues), issues)
	}
	if issues[0].Identifier != "external-123" || issues[0].Title != "Runtime item" {
		t.Fatalf("issue = %#v, want local-only runtime item", issues[0])
	}
}

func TestConnectorFetchIssueCommentsMergesRemoteAndLocalInCreatedOrder(t *testing.T) {
	t.Parallel()

	server := newGitHubLocalTestServer(t)
	localCreatedAt := time.Date(2026, 7, 2, 12, 3, 0, 0, time.UTC)
	conn, err := New(Config{
		GitHub: githubconnector.Config{
			Endpoint: server.URL + "/graphql",
			APIKey:   "ghp_test",
		},
		Local: local.Config{
			Path: filepath.Join(t.TempDir(), "comments.db"),
			Issues: []connector.Issue{{
				ID:         "github:123:779",
				Identifier: "digitaldrywood/detent#779",
				Title:      "Local issue",
				State:      "Todo",
				Metadata: map[string]string{
					local.MetadataGitHubNodeID:       "I_kwDOtest779",
					local.MetadataGitHubRepositoryID: "123",
					local.MetadataGitHubIssueNumber:  "779",
				},
				AssignedToWorker: true,
			}},
			Now: func() time.Time {
				return localCreatedAt
			},
		},
		Repository:     "digitaldrywood/detent",
		ActiveStates:   []string{"Todo", "In Progress"},
		TerminalStates: []string{"Done"},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	if err := conn.CreateComment(context.Background(), "github:123:779", "local note"); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}
	got, err := conn.FetchIssueComments(context.Background(), connector.Issue{
		ID:         "github:123:779",
		Identifier: "digitaldrywood/detent#779",
	})
	if err != nil {
		t.Fatalf("FetchIssueComments() error = %v", err)
	}

	wantBodies := []string{"remote earlier", "local note", "remote later"}
	if len(got) != len(wantBodies) {
		t.Fatalf("FetchIssueComments() len = %d, want %d: %#v", len(got), len(wantBodies), got)
	}
	for index, want := range wantBodies {
		if got[index].Body != want {
			t.Fatalf("comment[%d].Body = %q, want %q; comments = %#v", index, got[index].Body, want, got)
		}
	}
	if got[0].Local || !got[1].Local || got[2].Local {
		t.Fatalf("comment locality = [%v %v %v], want remote/local/remote", got[0].Local, got[1].Local, got[2].Local)
	}
	if got[1].Backend != connector.BackendLocalSQLite.String() || got[0].Backend != connector.BackendGitHub.String() {
		t.Fatalf("comment backends = %#v, want GitHub and local SQLite metadata", got)
	}
	if got[0].CanEdit || got[0].CanDelete || !got[1].CanEdit || !got[1].CanDelete || got[2].CanEdit || got[2].CanDelete {
		t.Fatalf("comment mutation flags = [%v/%v %v/%v %v/%v], want remote read-only and local mutable",
			got[0].CanEdit, got[0].CanDelete, got[1].CanEdit, got[1].CanDelete, got[2].CanEdit, got[2].CanDelete)
	}

	localCommentID := got[1].ID
	if err := conn.UpdateIssueComment(context.Background(), "github:123:779", localCommentID, "edited local note"); err != nil {
		t.Fatalf("UpdateIssueComment() error = %v", err)
	}
	got, err = conn.FetchIssueComments(context.Background(), connector.Issue{
		ID:         "github:123:779",
		Identifier: "digitaldrywood/detent#779",
	})
	if err != nil {
		t.Fatalf("FetchIssueComments() after edit error = %v", err)
	}
	wantBodies = []string{"remote earlier", "edited local note", "remote later"}
	for index, want := range wantBodies {
		if got[index].Body != want {
			t.Fatalf("comment[%d].Body after edit = %q, want %q; comments = %#v", index, got[index].Body, want, got)
		}
	}

	if err := conn.DeleteIssueComment(context.Background(), "github:123:779", localCommentID); err != nil {
		t.Fatalf("DeleteIssueComment() error = %v", err)
	}
	got, err = conn.FetchIssueComments(context.Background(), connector.Issue{
		ID:         "github:123:779",
		Identifier: "digitaldrywood/detent#779",
	})
	if err != nil {
		t.Fatalf("FetchIssueComments() after delete error = %v", err)
	}
	wantBodies = []string{"remote earlier", "remote later"}
	if len(got) != len(wantBodies) {
		t.Fatalf("FetchIssueComments() after delete len = %d, want %d: %#v", len(got), len(wantBodies), got)
	}
	for index, want := range wantBodies {
		if got[index].Body != want {
			t.Fatalf("comment[%d].Body after delete = %q, want %q; comments = %#v", index, got[index].Body, want, got)
		}
	}
}

const (
	localWriteThroughIssueID  = "github:123:779"
	githubWriteThroughIssueID = "I_kwDOtest779"
)

type writeThroughOperation struct {
	name         string
	githubMethod string
	githubCall   string
	localCall    string
	run          func(context.Context, *Connector) error
}

func writeThroughOperations() []writeThroughOperation {
	return []writeThroughOperation{
		{
			name:         "UpdateIssueState",
			githubMethod: "UpdateIssueState",
			githubCall:   call("github.UpdateIssueState", githubWriteThroughIssueID, "In Progress"),
			localCall:    call("local.UpdateIssueState", localWriteThroughIssueID, "In Progress"),
			run: func(ctx context.Context, conn *Connector) error {
				return conn.UpdateIssueState(ctx, localWriteThroughIssueID, "In Progress")
			},
		},
		{
			name:         "SetAssignee",
			githubMethod: "SetAssignee",
			githubCall:   call("github.SetAssignee", githubWriteThroughIssueID, "detent-bot"),
			localCall:    call("local.SetAssignee", localWriteThroughIssueID, "detent-bot"),
			run: func(ctx context.Context, conn *Connector) error {
				return conn.SetAssignee(ctx, localWriteThroughIssueID, "detent-bot")
			},
		},
		{
			name:         "SetField",
			githubMethod: "SetField",
			githubCall:   call("github.SetField", githubWriteThroughIssueID, "lease", "agent-1"),
			localCall:    call("local.SetField", localWriteThroughIssueID, "lease", "agent-1"),
			run: func(ctx context.Context, conn *Connector) error {
				return conn.SetField(ctx, localWriteThroughIssueID, "lease", "agent-1")
			},
		},
		{
			name:         "SetIssueField",
			githubMethod: "SetIssueField",
			githubCall:   call("github.SetIssueField", githubWriteThroughIssueID, "77", "claimed"),
			localCall:    call("local.SetIssueField", localWriteThroughIssueID, "77", "claimed"),
			run: func(ctx context.Context, conn *Connector) error {
				return conn.SetIssueField(ctx, localWriteThroughIssueID, 77, "claimed")
			},
		},
		{
			name:         "ClearIssueField",
			githubMethod: "ClearIssueField",
			githubCall:   call("github.ClearIssueField", githubWriteThroughIssueID, "77"),
			localCall:    call("local.ClearIssueField", localWriteThroughIssueID, "77"),
			run: func(ctx context.Context, conn *Connector) error {
				return conn.ClearIssueField(ctx, localWriteThroughIssueID, 77)
			},
		},
		{
			name:         "CloseIssue",
			githubMethod: "CloseIssue",
			githubCall:   call("github.CloseIssue", githubWriteThroughIssueID),
			localCall:    call("local.CloseIssue", localWriteThroughIssueID),
			run: func(ctx context.Context, conn *Connector) error {
				return conn.CloseIssue(ctx, localWriteThroughIssueID)
			},
		},
		{
			name:         "RemoveIssueFromProject",
			githubMethod: "RemoveIssueFromProject",
			githubCall:   call("github.RemoveIssueFromProject", githubWriteThroughIssueID),
			localCall:    call("local.RemoveIssueFromProject", localWriteThroughIssueID),
			run: func(ctx context.Context, conn *Connector) error {
				return conn.RemoveIssueFromProject(ctx, localWriteThroughIssueID)
			},
		},
	}
}

func newRecordingWriteThroughConnector(t *testing.T, githubErrs map[string]error, localErrs map[string]error) (*Connector, *recordingCallLog) {
	t.Helper()
	calls := &recordingCallLog{}
	return &Connector{
		github: &recordingGitHubBackend{calls: calls, errs: githubErrs},
		local: &recordingLocalBackend{
			calls: calls,
			errs:  localErrs,
			issues: map[string]connector.Issue{
				localWriteThroughIssueID: {
					ID:         localWriteThroughIssueID,
					Identifier: "digitaldrywood/detent#779",
					Metadata: map[string]string{
						local.MetadataGitHubNodeID: githubWriteThroughIssueID,
					},
				},
			},
		},
	}, calls
}

func localLookupCall() string {
	return call("local.FetchIssueStatesByIDs", localWriteThroughIssueID)
}

func call(method string, args ...string) string {
	return method + "(" + strings.Join(args, ",") + ")"
}

type recordingCallLog struct {
	calls []string
}

func (l *recordingCallLog) add(method string, args ...string) {
	l.calls = append(l.calls, call(method, args...))
}

func (l *recordingCallLog) snapshot() []string {
	return append([]string(nil), l.calls...)
}

type recordingGitHubBackend struct {
	calls *recordingCallLog
	errs  map[string]error
}

func (b *recordingGitHubBackend) Close() error {
	return nil
}

func (b *recordingGitHubBackend) Authenticate(context.Context) error {
	return nil
}

func (b *recordingGitHubBackend) InstanceLogin() string {
	return ""
}

func (b *recordingGitHubBackend) GraphQLRateLimit() (connector.GraphQLRateLimit, bool) {
	return connector.GraphQLRateLimit{}, false
}

func (b *recordingGitHubBackend) AuthHealth() (connector.AuthHealth, bool) {
	return connector.AuthHealth{}, false
}

func (b *recordingGitHubBackend) ResetGraphQLRateLimitUsage() {}

func (b *recordingGitHubBackend) FlushGraphQLRateLimitUsage() connector.GraphQLRateLimitUsage {
	return connector.GraphQLRateLimitUsage{}
}

func (b *recordingGitHubBackend) FlushRESTRateLimitUsage() connector.RESTRateLimitUsage {
	return connector.RESTRateLimitUsage{}
}

func (b *recordingGitHubBackend) FetchIssueStatesByIdentifiers(context.Context, []string) ([]connector.Issue, error) {
	panic("unexpected github FetchIssueStatesByIdentifiers")
}

func (b *recordingGitHubBackend) FetchRepositoryInfo(context.Context, string) (githubconnector.RepositoryInfo, error) {
	panic("unexpected github FetchRepositoryInfo")
}

func (b *recordingGitHubBackend) FetchIssueComments(context.Context, connector.Issue) ([]connector.IssueComment, error) {
	panic("unexpected github FetchIssueComments")
}

func (b *recordingGitHubBackend) CreatePullRequestComment(context.Context, string, int, string) error {
	panic("unexpected github CreatePullRequestComment")
}

func (b *recordingGitHubBackend) FetchPullRequestComments(context.Context, string, int) ([]connector.IssueComment, error) {
	panic("unexpected github FetchPullRequestComments")
}

func (b *recordingGitHubBackend) MergePullRequest(context.Context, string, int, string, string) error {
	panic("unexpected github MergePullRequest")
}

func (b *recordingGitHubBackend) InspectPullRequestMergeQueue(context.Context, connector.Issue) (connector.PullRequestMergeQueueStatus, error) {
	panic("unexpected github InspectPullRequestMergeQueue")
}

func (b *recordingGitHubBackend) EnqueuePullRequest(context.Context, connector.Issue) (connector.PullRequestMergeQueueEntry, error) {
	panic("unexpected github EnqueuePullRequest")
}

func (b *recordingGitHubBackend) HydratePullRequest(context.Context, connector.Issue) (connector.Issue, error) {
	panic("unexpected github HydratePullRequest")
}

func (b *recordingGitHubBackend) FetchIssueParents(context.Context, string) ([]connector.Issue, error) {
	panic("unexpected github FetchIssueParents")
}

func (b *recordingGitHubBackend) FetchIssueChildren(context.Context, string) ([]connector.BlockedRef, error) {
	panic("unexpected github FetchIssueChildren")
}

func (b *recordingGitHubBackend) UpdateIssueState(_ context.Context, issueID string, stateName string) error {
	b.calls.add("github.UpdateIssueState", issueID, stateName)
	return b.err("UpdateIssueState")
}

func (b *recordingGitHubBackend) SetAssignee(_ context.Context, issueID string, login string) error {
	b.calls.add("github.SetAssignee", issueID, login)
	return b.err("SetAssignee")
}

func (b *recordingGitHubBackend) SetField(_ context.Context, issueID string, fieldName string, value string) error {
	b.calls.add("github.SetField", issueID, fieldName, value)
	return b.err("SetField")
}

func (b *recordingGitHubBackend) SetIssueField(_ context.Context, issueID string, fieldID int, value string) error {
	b.calls.add("github.SetIssueField", issueID, strconv.Itoa(fieldID), value)
	return b.err("SetIssueField")
}

func (b *recordingGitHubBackend) ClearIssueField(_ context.Context, issueID string, fieldID int) error {
	b.calls.add("github.ClearIssueField", issueID, strconv.Itoa(fieldID))
	return b.err("ClearIssueField")
}

func (b *recordingGitHubBackend) CloseIssue(_ context.Context, issueID string) error {
	b.calls.add("github.CloseIssue", issueID)
	return b.err("CloseIssue")
}

func (b *recordingGitHubBackend) RemoveIssueFromProject(_ context.Context, issueID string) error {
	b.calls.add("github.RemoveIssueFromProject", issueID)
	return b.err("RemoveIssueFromProject")
}

func (b *recordingGitHubBackend) err(method string) error {
	if b.errs == nil {
		return nil
	}
	return b.errs[method]
}

type recordingLocalBackend struct {
	calls  *recordingCallLog
	errs   map[string]error
	issues map[string]connector.Issue
}

func (b *recordingLocalBackend) Name() string {
	return connector.BackendLocalSQLite.String()
}

func (b *recordingLocalBackend) Close() error {
	return nil
}

func (b *recordingLocalBackend) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	panic("unexpected local FetchCandidateIssues")
}

func (b *recordingLocalBackend) FetchCandidateIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	panic("unexpected local FetchCandidateIssuesByStates")
}

func (b *recordingLocalBackend) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	panic("unexpected local FetchIssuesByStates")
}

func (b *recordingLocalBackend) FetchIssuesByStatesLimit(context.Context, []string, int) ([]connector.Issue, error) {
	panic("unexpected local FetchIssuesByStatesLimit")
}

func (b *recordingLocalBackend) FetchIssueStateProbe(context.Context, []string, int) ([]connector.Issue, error) {
	panic("unexpected local FetchIssueStateProbe")
}

func (b *recordingLocalBackend) FetchIssueStatesByIDs(_ context.Context, issueIDs []string) ([]connector.Issue, error) {
	b.calls.add("local.FetchIssueStatesByIDs", issueIDs...)
	if err := b.err("FetchIssueStatesByIDs"); err != nil {
		return nil, err
	}
	out := make([]connector.Issue, 0, len(issueIDs))
	for _, issueID := range issueIDs {
		if issue, ok := b.issues[strings.TrimSpace(issueID)]; ok {
			out = append(out, issue)
		}
	}
	return out, nil
}

func (b *recordingLocalBackend) FetchIssueStatesByIdentifiers(context.Context, []string) ([]connector.Issue, error) {
	panic("unexpected local FetchIssueStatesByIdentifiers")
}

func (b *recordingLocalBackend) FetchIssueComments(context.Context, connector.Issue) ([]connector.IssueComment, error) {
	panic("unexpected local FetchIssueComments")
}

func (b *recordingLocalBackend) CreateComment(context.Context, string, string) error {
	panic("unexpected local CreateComment")
}

func (b *recordingLocalBackend) UpdateIssueComment(context.Context, string, string, string) error {
	panic("unexpected local UpdateIssueComment")
}

func (b *recordingLocalBackend) DeleteIssueComment(context.Context, string, string) error {
	panic("unexpected local DeleteIssueComment")
}

func (b *recordingLocalBackend) UpsertIssues(context.Context, []connector.Issue) error {
	panic("unexpected local UpsertIssues")
}

func (b *recordingLocalBackend) UpdateIssueState(_ context.Context, issueID string, stateName string) error {
	b.calls.add("local.UpdateIssueState", issueID, stateName)
	return b.err("UpdateIssueState")
}

func (b *recordingLocalBackend) SetAssignee(_ context.Context, issueID string, login string) error {
	b.calls.add("local.SetAssignee", issueID, login)
	return b.err("SetAssignee")
}

func (b *recordingLocalBackend) SetField(_ context.Context, issueID string, fieldName string, value string) error {
	b.calls.add("local.SetField", issueID, fieldName, value)
	return b.err("SetField")
}

func (b *recordingLocalBackend) SetIssueField(_ context.Context, issueID string, fieldID int, value string) error {
	b.calls.add("local.SetIssueField", issueID, strconv.Itoa(fieldID), value)
	return b.err("SetIssueField")
}

func (b *recordingLocalBackend) ClearIssueField(_ context.Context, issueID string, fieldID int) error {
	b.calls.add("local.ClearIssueField", issueID, strconv.Itoa(fieldID))
	return b.err("ClearIssueField")
}

func (b *recordingLocalBackend) CloseIssue(_ context.Context, issueID string) error {
	b.calls.add("local.CloseIssue", issueID)
	return b.err("CloseIssue")
}

func (b *recordingLocalBackend) RemoveIssueFromProject(_ context.Context, issueID string) error {
	b.calls.add("local.RemoveIssueFromProject", issueID)
	return b.err("RemoveIssueFromProject")
}

func (b *recordingLocalBackend) err(method string) error {
	if b.errs == nil {
		return nil
	}
	return b.errs[method]
}

type githubLocalTestRequest struct {
	Method string
	Path   string
}

type githubLocalTestServer struct {
	*httptest.Server
	t        *testing.T
	mu       sync.Mutex
	requests []githubLocalTestRequest
}

func newGitHubLocalTestServer(t *testing.T) *githubLocalTestServer {
	t.Helper()
	testServer := &githubLocalTestServer{t: t}
	server := httptest.NewServer(http.HandlerFunc(testServer.handle))
	testServer.Server = server
	t.Cleanup(server.Close)
	return testServer
}

func (s *githubLocalTestServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.mu.Lock()
		s.requests = append(s.requests, githubLocalTestRequest{Method: r.Method, Path: r.URL.Path})
		s.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/digitaldrywood/detent":
		writeGitHubLocalJSON(s.t, w, map[string]any{
			"id":        123,
			"full_name": "digitaldrywood/detent",
			"html_url":  "https://github.com/digitaldrywood/detent",
		})
	case r.Method == http.MethodGet && r.URL.Path == "/repos/digitaldrywood/detent/issues/779":
		body := "Depends on: #1"
		writeGitHubLocalJSON(s.t, w, map[string]any{
			"node_id":      "I_kwDOtest779",
			"number":       779,
			"title":        "Closed upstream issue",
			"body":         body,
			"state":        "closed",
			"state_reason": "completed",
			"html_url":     "https://github.com/digitaldrywood/detent/issues/779",
			"created_at":   "2026-07-01T12:00:00Z",
			"updated_at":   "2026-07-02T12:00:00Z",
			"user":         map[string]any{"login": "octocat"},
			"assignees":    []map[string]any{{"node_id": "U_1", "login": "detent-bot"}},
			"labels":       []map[string]string{{"name": "enhancement"}},
		})
	case r.Method == http.MethodGet && r.URL.Path == "/repos/digitaldrywood/detent/issues/779/comments":
		writeGitHubLocalJSON(s.t, w, []map[string]any{
			{
				"id":         1001,
				"node_id":    "IC_remote_early",
				"body":       "remote earlier",
				"html_url":   "https://github.com/digitaldrywood/detent/issues/779#issuecomment-1001",
				"created_at": "2026-07-02T12:00:00Z",
				"updated_at": "2026-07-02T12:00:00Z",
				"user":       map[string]any{"login": "octocat"},
			},
			{
				"id":         1002,
				"node_id":    "IC_remote_late",
				"body":       "remote later",
				"html_url":   "https://github.com/digitaldrywood/detent/issues/779#issuecomment-1002",
				"created_at": "2026-07-02T12:05:00Z",
				"updated_at": "2026-07-02T12:05:00Z",
				"user":       map[string]any{"login": "octocat"},
			},
		})
	case r.Method == http.MethodGet && r.URL.Path == "/repos/digitaldrywood/detent/pulls":
		writeGitHubLocalJSON(s.t, w, []map[string]any{})
	case r.Method == http.MethodPost && r.URL.Path == "/repos/digitaldrywood/detent/issues/12/comments":
		writeGitHubLocalJSON(s.t, w, map[string]any{"node_id": "comment-node"})
	case r.Method == http.MethodPut && r.URL.Path == "/repos/digitaldrywood/detent/pulls/12/merge":
		writeGitHubLocalJSON(s.t, w, map[string]any{"sha": "merge-sha", "merged": true, "message": "ok"})
	default:
		http.NotFound(w, r)
	}
}

func (s *githubLocalTestServer) writeRequests() []githubLocalTestRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]githubLocalTestRequest(nil), s.requests...)
}

func writeGitHubLocalJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
