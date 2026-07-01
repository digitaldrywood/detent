package connector

import (
	"context"
	"errors"
)

var ErrNotImplemented = errors.New("connector operation not implemented")

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

type IssueCommentReader interface {
	FetchIssueComments(context.Context, Issue) ([]IssueComment, error)
}

type IssuesByStatesLimiter interface {
	FetchIssuesByStatesLimit(context.Context, []string, int) ([]Issue, error)
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

type IssueReferenceResolver interface {
	FetchIssueStatesByIdentifiers(context.Context, []string) ([]Issue, error)
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
