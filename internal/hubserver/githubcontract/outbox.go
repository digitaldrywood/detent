package githubcontract

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"time"
)

const (
	MutationWorkflowLabel    MutationKind = "workflow_label"
	MutationWorkpad          MutationKind = "workpad"
	MutationMergePullRequest MutationKind = "merge_pull_request"
)

type MutationKind string

func (k MutationKind) Irreversible() bool {
	return k == MutationMergePullRequest
}

type WorkflowLabelDesired struct {
	Label         string `json:"label"`
	ManagedPrefix string `json:"managed_prefix"`
	// Clear is set where the managed prefix is to be emptied. It is absent
	// from every record written before it existed, which decodes to false and
	// keeps those records meaning what they meant.
	Clear bool `json:"clear,omitempty"`
}

type WorkpadDesired struct {
	Summary     bool   `json:"summary,omitempty"`
	CloseSource bool   `json:"close_source,omitempty"`
	StateReason string `json:"state_reason,omitempty"`
	IssueNumber int    `json:"issue_number,omitempty"`
	Phase       string `json:"phase"`
	Body        string `json:"body"`
	Marker      string `json:"marker"`
}

type MergePullRequestDesired struct {
	PullRequestNumber int    `json:"pull_request_number"`
	HeadSHA           string `json:"head_sha"`
	MergeMethod       string `json:"merge_method"`
}

type OutboxItem struct {
	OrganizationID  tracker.OrganizationID
	Profile         string
	ID              int64
	IdempotencyKey  string
	RepositoryID    int64
	RepositoryOwner string
	RepositoryName  string
	IssueID         int64
	IssueNumber     int
	Kind            MutationKind
	TargetNodeID    string
	Desired         json.RawMessage
	Status          string
	Attempts        int
	CreatedAt       time.Time
	Progress        OutboxProgress
	Uncertain       bool
	Blocked         bool
	BeforeWrite     func(context.Context) error `json:"-"`
}

type OutboxProgress = projectstore.GitHubOutboundProgress
type OutboxObservation = projectstore.GitHubOutboundObservation

type OutboxStepResult struct {
	Progress     OutboxProgress
	Observations []OutboxObservation
	Done         bool
}

type OutboxStepBackend interface {
	Step(context.Context, OutboxItem) (OutboxStepResult, error)
}

type OutboxBackend interface {
	Execute(context.Context, OutboxItem) error
	VerifyAndExecute(context.Context, OutboxItem) error
}

var ErrOutboxUncertain = errors.New("GitHub write requires reconciliation")

type rejectedOutboxWrite struct{ error }

func (e *rejectedOutboxWrite) Unwrap() error { return e.error }

func RejectedOutboxWrite(err error) error {
	if err == nil {
		return nil
	}
	return &rejectedOutboxWrite{err}
}

func IsRejectedOutboxWrite(err error) bool {
	var rejected *rejectedOutboxWrite
	return errors.As(err, &rejected)
}

type permanentError struct {
	err error
}

func (e *permanentError) Error() string {
	return e.err.Error()
}

func (e *permanentError) Unwrap() error {
	return e.err
}

func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

func IsPermanent(err error) bool {
	var target *permanentError
	return errors.As(err, &target)
}
