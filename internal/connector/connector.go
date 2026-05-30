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
}

type Authenticator interface {
	Authenticate(context.Context) error
}

type Provisioner interface {
	Provision(context.Context) error
}
