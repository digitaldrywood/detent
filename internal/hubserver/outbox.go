package hubserver

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"time"
)

const (
	outboxPending      = "pending"
	outboxProcessing   = "processing"
	outboxRetrying     = "retrying"
	outboxCompleted    = "completed"
	outboxDeadLetter   = "dead_letter"
	outboxSuperseded   = "superseded"
	workpadMarker      = "<!-- detent-workpad -->"
	defaultLabelPrefix = "detent:"
	maxOutboxErrorLen  = 2000
)

var ErrIdempotencyConflict = errors.New("github outbox idempotency key already has different content")

type GitHubMutation interface {
	outboxRecord() (outboxRecord, error)
}

type WorkflowLabelMutation struct {
	IdempotencyKey string
	RepositoryID   int64
	IssueID        int64
	Label          string
	ManagedPrefix  string
	// Clear removes every label under ManagedPrefix and adds none. It is how
	// a managed field is emptied — a priority taken off an issue — rather
	// than moved from one value to another.
	Clear bool
}

type WorkpadMutation struct {
	IdempotencyKey string
	RepositoryID   int64
	IssueID        int64
	Phase          string
	Body           string
}

type MergePullRequestMutation struct {
	IdempotencyKey    string
	RepositoryID      int64
	IssueID           int64
	PullRequestNumber int
	HeadSHA           string
	MergeMethod       string
}

type WorkEventChange struct {
	IssueID      int64
	FencingToken *int64
	MachineID    string
	SessionID    string
	RunID        string
	Kind         string
	Payload      json.RawMessage
	OccurredAt   time.Time
	Mutation     GitHubMutation
}

type WorkflowStateChange struct {
	IssueID         int64
	WorkflowStateID int64
	Mutation        WorkflowLabelMutation
}

type OutboxHealth struct {
	Pending         int              `json:"pending"`
	Retrying        int              `json:"retrying"`
	Processing      int              `json:"processing"`
	DeadLetters     int              `json:"dead_letters"`
	OldestPendingAt *time.Time       `json:"oldest_pending_at,omitempty"`
	OperatorActions []OperatorAction `json:"operator_actions,omitempty"`
}

type OperatorAction struct {
	ID             int64        `json:"id"`
	IdempotencyKey string       `json:"idempotency_key"`
	Kind           MutationKind `json:"kind"`
	TargetNodeID   string       `json:"target_node_id,omitempty"`
	Action         string       `json:"action"`
}

type outboxRecord struct {
	idempotencyKey string
	repositoryID   int64
	issueID        int64
	kind           MutationKind
	targetNodeID   string
	desired        json.RawMessage
	coalesceKey    string
}

func (s *Service) ChangeWorkflowState(ctx context.Context, change WorkflowStateChange) (OutboxItem, error) {
	return OutboxItem{}, ErrOutboxDisabled
}

func (s *Service) AppendWorkEvent(ctx context.Context, change WorkEventChange) (OutboxItem, error) {
	return OutboxItem{}, ErrOutboxDisabled
}

func (s *Service) commitOutbox(ctx context.Context, record outboxRecord, apply func(*sql.Tx, string) error) (result OutboxItem, resultErr error) {
	return OutboxItem{}, ErrOutboxDisabled
}

func (s *Service) ProcessOutbox(ctx context.Context) (bool, error) {
	return s.processGitHubOutboundRecord(ctx)
}

func (s *Service) claimOutbox(ctx context.Context) (result OutboxItem, found bool, resultErr error) {
	return OutboxItem{}, false, ErrOutboxDisabled
}

func randomJitter() float64 {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return 0.5
	}
	return float64(binary.LittleEndian.Uint64(value[:])>>11) / (1 << 53)
}

func (s *Service) OutboxHealth(ctx context.Context) (OutboxHealth, error) {
	return OutboxHealth{}, ErrOutboxDisabled
}
