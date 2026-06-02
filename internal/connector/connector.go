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

type IssueReferenceResolver interface {
	FetchIssueStatesByIdentifiers(context.Context, []string) ([]Issue, error)
}
