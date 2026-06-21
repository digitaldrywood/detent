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

type IssueFieldSetter interface {
	SetIssueField(context.Context, string, int, string) error
}

type PullRequestCommenter interface {
	CreatePullRequestComment(context.Context, string, int, string) error
}

type IssueCommentReader interface {
	FetchIssueComments(context.Context, Issue) ([]IssueComment, error)
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
