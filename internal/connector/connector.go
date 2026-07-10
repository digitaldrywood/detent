package connector

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotImplemented     = errors.New("connector operation not implemented")
	ErrStateUpdateBlocked = errors.New("issue state update blocked")
)

type StateUpdateBlockedError struct {
	IssueID      string
	CurrentState string
	TargetState  string
}

func (e *StateUpdateBlockedError) Error() string {
	if e == nil {
		return ErrStateUpdateBlocked.Error()
	}
	return ErrStateUpdateBlocked.Error() + ": " + e.IssueID + " is in terminal state " + e.CurrentState + "; refusing move to non-terminal " + e.TargetState
}

func (e *StateUpdateBlockedError) Is(target error) bool {
	return target == ErrStateUpdateBlocked
}

type Connector interface {
	Name() string
	FetchCandidateIssues(context.Context) ([]Issue, error)
	FetchIssuesByStates(context.Context, []string) ([]Issue, error)
	FetchIssueStatesByIDs(context.Context, []string) ([]Issue, error)
	CreateComment(context.Context, string, string) error
	UpdateIssueState(context.Context, string, string) error
	SetAssignee(context.Context, string, string) error
	SetField(context.Context, string, string, string) error
}

type IssueFilterHint struct {
	Authors      []string
	Assignees    []string
	LabelInclude []string
	LabelExclude []string
}

type CandidateIssuesFilterFetcher interface {
	FetchCandidateIssuesByStatesWithFilter(context.Context, []string, IssueFilterHint) ([]Issue, error)
}

type Closer interface {
	Close() error
}

type Authenticator interface {
	Authenticate(context.Context) error
}

type InstanceIdentifier interface {
	InstanceLogin() string
}

type Provisioner interface {
	Provision(context.Context) error
}

type IssueCloser interface {
	CloseIssue(context.Context, string) error
}

type ProjectRemover interface {
	RemoveIssueFromProject(context.Context, string) error
}

type IssueFieldSetter interface {
	SetIssueField(context.Context, string, int, string) error
}

type IssueFieldClearer interface {
	ClearIssueField(context.Context, string, int) error
}

type PullRequestCommenter interface {
	CreatePullRequestComment(context.Context, string, int, string) error
}

type PullRequestMerger interface {
	MergePullRequest(context.Context, string, int, string) error
}

type PullRequestHydrator interface {
	HydratePullRequest(context.Context, Issue) (Issue, error)
}

type PullRequestCheckRerunner interface {
	RerunPullRequestChecks(context.Context, Issue, []PullRequestCheck) error
}

type IssueCommentReader interface {
	FetchIssueComments(context.Context, Issue) ([]IssueComment, error)
}

type IssueCommentUpdater interface {
	UpdateIssueComment(context.Context, string, string, string) error
}

type IssueCommentDeleter interface {
	DeleteIssueComment(context.Context, string, string) error
}

type PullRequestCommentReader interface {
	FetchPullRequestComments(context.Context, string, int) ([]IssueComment, error)
}

type IssueDraft struct {
	Title  string
	Body   string
	Labels []string
}

type IssueCreator interface {
	CreateIssue(context.Context, IssueDraft) (Issue, error)
}

type IssuesByStatesLimiter interface {
	FetchIssuesByStatesLimit(context.Context, []string, int) ([]Issue, error)
}

type IssueStateScan struct {
	Issues           []Issue        `json:"issues,omitempty"`
	BoardCounts      map[string]int `json:"board_counts,omitempty"`
	EnumeratedCounts map[string]int `json:"enumerated_counts,omitempty"`
	ItemsFetched     int            `json:"items_fetched,omitempty"`
	TotalItems       int            `json:"total_items,omitempty"`
}

type IssueStateScanner interface {
	FetchIssuesByStatesScan(context.Context, []string, int) (IssueStateScan, error)
}

type CandidateIssuesByStatesFetcher interface {
	FetchCandidateIssuesByStates(context.Context, []string) ([]Issue, error)
}

type IssueStateProber interface {
	FetchIssueStateProbe(context.Context, []string, int) ([]Issue, error)
}

type StatusDriftReader interface {
	FetchStatusDrift(context.Context) (StatusDrift, error)
}

type ConditionalPoller interface {
	ConditionalPollingEnabled() bool
}

type IssueReferenceResolver interface {
	FetchIssueStatesByIdentifiers(context.Context, []string) ([]Issue, error)
}

type IssueStateTransitionReader interface {
	IssueStateEnteredAt(context.Context, Issue) (time.Time, bool, error)
}

type IssueDependencyWriter interface {
	AddIssueBlockedByDependency(context.Context, string, string) error
	RemoveIssueBlockedByDependency(context.Context, string, string) error
}

type IssueUpserter interface {
	UpsertIssues(context.Context, []Issue) error
}

type IssueParentResolver interface {
	FetchIssueParents(context.Context, string) ([]Issue, error)
}

type IssueChildrenResolver interface {
	FetchIssueChildren(context.Context, string) ([]BlockedRef, error)
}

type StatusDrift struct {
	UntrackedOpen []Issue `json:"untracked_open,omitempty" yaml:"untracked_open,omitempty"`
	OpenTerminal  []Issue `json:"open_terminal,omitempty" yaml:"open_terminal,omitempty"`
}
