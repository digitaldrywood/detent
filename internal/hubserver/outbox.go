package hubserver

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	MutationWorkflowLabel    MutationKind = "workflow_label"
	MutationWorkpad          MutationKind = "workpad"
	MutationMergePullRequest MutationKind = "merge_pull_request"

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

type MutationKind string

func (k MutationKind) irreversible() bool {
	return k == MutationMergePullRequest
}

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

type OutboxItem struct {
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
}

type OutboxBackend interface {
	Execute(context.Context, OutboxItem) error
	VerifyAndExecute(context.Context, OutboxItem) error
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

type outboxRecord struct {
	idempotencyKey string
	repositoryID   int64
	issueID        int64
	kind           MutationKind
	targetNodeID   string
	desired        json.RawMessage
	coalesceKey    string
}

func (m WorkflowLabelMutation) outboxRecord() (outboxRecord, error) {
	prefix := strings.TrimSpace(m.ManagedPrefix)
	if prefix == "" {
		prefix = defaultLabelPrefix
	}
	if strings.TrimSpace(m.Label) == "" && !m.Clear {
		return outboxRecord{}, errors.New("workflow label is required")
	}
	desired, err := json.Marshal(WorkflowLabelDesired{
		Label:         strings.TrimSpace(m.Label),
		ManagedPrefix: prefix,
		Clear:         m.Clear,
	})
	if err != nil {
		return outboxRecord{}, err
	}
	record := outboxRecord{
		idempotencyKey: strings.TrimSpace(m.IdempotencyKey),
		repositoryID:   m.RepositoryID,
		issueID:        m.IssueID,
		kind:           MutationWorkflowLabel,
		desired:        desired,
		coalesceKey:    fmt.Sprintf("managed-label:%d:%d:%s", m.RepositoryID, m.IssueID, strings.ToLower(prefix)),
	}
	return record, record.validate()
}

func (m WorkpadMutation) outboxRecord() (outboxRecord, error) {
	phase := strings.ToLower(strings.TrimSpace(m.Phase))
	desired, err := json.Marshal(WorkpadDesired{
		Phase:  phase,
		Body:   strings.TrimSpace(m.Body),
		Marker: workpadMarker,
	})
	if err != nil {
		return outboxRecord{}, err
	}
	record := outboxRecord{
		idempotencyKey: strings.TrimSpace(m.IdempotencyKey),
		repositoryID:   m.RepositoryID,
		issueID:        m.IssueID,
		kind:           MutationWorkpad,
		desired:        desired,
		coalesceKey:    fmt.Sprintf("workpad:%d:%d", m.RepositoryID, m.IssueID),
	}
	if phase == "" {
		return outboxRecord{}, errors.New("workpad phase is required")
	}
	if strings.TrimSpace(m.Body) == "" {
		return outboxRecord{}, errors.New("workpad body is required")
	}
	return record, record.validate()
}

func (m MergePullRequestMutation) outboxRecord() (outboxRecord, error) {
	method := strings.ToLower(strings.TrimSpace(m.MergeMethod))
	if method == "" {
		method = "squash"
	}
	switch method {
	case "merge", "rebase", "squash":
	default:
		return outboxRecord{}, errors.New("merge method must be merge, rebase, or squash")
	}
	if m.PullRequestNumber <= 0 {
		return outboxRecord{}, errors.New("pull request number must be positive")
	}
	if strings.TrimSpace(m.HeadSHA) == "" {
		return outboxRecord{}, errors.New("pull request head SHA is required")
	}
	desired, err := json.Marshal(MergePullRequestDesired{
		PullRequestNumber: m.PullRequestNumber,
		HeadSHA:           strings.TrimSpace(m.HeadSHA),
		MergeMethod:       method,
	})
	if err != nil {
		return outboxRecord{}, err
	}
	record := outboxRecord{
		idempotencyKey: strings.TrimSpace(m.IdempotencyKey),
		repositoryID:   m.RepositoryID,
		issueID:        m.IssueID,
		kind:           MutationMergePullRequest,
		targetNodeID:   fmt.Sprintf("pull-request:%d", m.PullRequestNumber),
		desired:        desired,
	}
	return record, record.validate()
}

func (r outboxRecord) validate() error {
	if r.idempotencyKey == "" {
		return errors.New("github mutation idempotency key is required")
	}
	if r.repositoryID <= 0 {
		return errors.New("github mutation repository id must be positive")
	}
	if r.issueID <= 0 {
		return errors.New("github mutation issue id must be positive")
	}
	if !json.Valid(r.desired) {
		return errors.New("github mutation desired state must be valid JSON")
	}
	return nil
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

func (i OutboxItem) matches(record outboxRecord) bool {
	return i.IdempotencyKey == record.idempotencyKey &&
		i.RepositoryID == record.repositoryID &&
		i.IssueID == record.issueID &&
		i.Kind == record.kind &&
		i.TargetNodeID == record.targetNodeID &&
		jsonEqual(i.Desired, record.desired)
}

func jsonEqual(left, right json.RawMessage) bool {
	var leftValue any
	var rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	leftJSON, err := json.Marshal(leftValue)
	if err != nil {
		return false
	}
	rightJSON, err := json.Marshal(rightValue)
	if err != nil {
		return false
	}
	return string(leftJSON) == string(rightJSON)
}

func (s *Service) ProcessOutbox(ctx context.Context) (bool, error) {
	return false, ErrOutboxDisabled
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

func formatOutboxTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func parseOutboxTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}

func truncateOutboxError(message string) string {
	message = strings.TrimSpace(message)
	if len(message) <= maxOutboxErrorLen {
		return message
	}
	return message[:maxOutboxErrorLen]
}

type outboxWorker struct {
	service *Service
	cancel  context.CancelFunc
	wake    chan struct{}
	wait    sync.WaitGroup
}

func newOutboxWorker(service *Service) *outboxWorker {
	return &outboxWorker{
		service: service,
		wake:    make(chan struct{}, 1),
	}
}

func (w *outboxWorker) start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	w.cancel = cancel
	w.wait.Add(1)
	go func() {
		defer w.wait.Done()
		w.run(ctx)
	}()
}

func (w *outboxWorker) run(ctx context.Context) {
	ticker := time.NewTicker(w.service.config.OutboxPollInterval)
	defer ticker.Stop()
	for {
		for {
			processed, err := w.service.processSlackDelivery(ctx)
			if err != nil && ctx.Err() == nil {
				w.service.config.Logger.WarnContext(ctx, "Slack delivery storage failed", "error", err)
			}
			githubProcessed := false
			if w.service.config.OutboxBackend != nil {
				githubProcessed, err = w.service.ProcessOutbox(ctx)
			} else {
				err = nil
			}
			processed = processed || githubProcessed
			if err != nil && !errors.Is(err, context.Canceled) {
				w.service.config.Logger.WarnContext(ctx, "github outbox delivery failed", "error", err)
			}
			if !processed || ctx.Err() != nil {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-ticker.C:
		}
	}
}

func (w *outboxWorker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *outboxWorker) stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.wait.Wait()
}
