package hubserver

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
}

type WorkpadDesired struct {
	Phase  string `json:"phase"`
	Body   string `json:"body"`
	Marker string `json:"marker"`
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
	if strings.TrimSpace(m.Label) == "" {
		return outboxRecord{}, errors.New("workflow label is required")
	}
	desired, err := json.Marshal(WorkflowLabelDesired{
		Label:         strings.TrimSpace(m.Label),
		ManagedPrefix: prefix,
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
	if !s.ready.Load() {
		return OutboxItem{}, ErrNotReady
	}
	record, err := change.Mutation.outboxRecord()
	if err != nil {
		return OutboxItem{}, err
	}
	if change.IssueID <= 0 || change.WorkflowStateID <= 0 {
		return OutboxItem{}, errors.New("issue and workflow state ids must be positive")
	}
	if change.IssueID != record.issueID {
		return OutboxItem{}, errors.New("workflow state issue does not match github mutation issue")
	}

	return s.commitOutbox(ctx, record, func(tx *sql.Tx, now string) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE issues
			SET workflow_state_id = ?, updated_at = ?
			WHERE id = ? AND repository_id = ?
			  AND EXISTS (
				SELECT 1 FROM workflow_states
				WHERE id = ? AND repository_id = ?
			  )`,
			change.WorkflowStateID, now, change.IssueID, record.repositoryID,
			change.WorkflowStateID, record.repositoryID,
		)
		if err != nil {
			return fmt.Errorf("update hub workflow state: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read hub workflow state update result: %w", err)
		}
		if rows != 1 {
			return errors.New("hub issue or workflow state was not found in the mutation repository")
		}
		return nil
	})
}

func (s *Service) AppendWorkEvent(ctx context.Context, change WorkEventChange) (OutboxItem, error) {
	if !s.ready.Load() {
		return OutboxItem{}, ErrNotReady
	}
	if change.Mutation == nil {
		return OutboxItem{}, errors.New("github mutation is required for a work event")
	}
	record, err := change.Mutation.outboxRecord()
	if err != nil {
		return OutboxItem{}, err
	}
	if change.IssueID <= 0 || change.IssueID != record.issueID {
		return OutboxItem{}, errors.New("work event issue does not match github mutation issue")
	}
	kind := strings.TrimSpace(change.Kind)
	if kind == "" {
		return OutboxItem{}, errors.New("work event kind is required")
	}
	payload := change.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if !json.Valid(payload) {
		return OutboxItem{}, errors.New("work event payload must be valid JSON")
	}
	occurredAt := change.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = s.config.now()
	}

	return s.commitOutbox(ctx, record, func(tx *sql.Tx, now string) error {
		var repositoryID int64
		if err := tx.QueryRowContext(ctx, "SELECT repository_id FROM issues WHERE id = ?", change.IssueID).Scan(&repositoryID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("hub issue was not found")
			}
			return fmt.Errorf("read hub work event issue: %w", err)
		}
		if repositoryID != record.repositoryID {
			return errors.New("hub work event issue is not in the mutation repository")
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO work_events (
				issue_id, fencing_token, machine_id, session_id, run_id,
				kind, payload_json, occurred_at, recorded_at
			) VALUES (?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?)`,
			change.IssueID, change.FencingToken, strings.TrimSpace(change.MachineID),
			strings.TrimSpace(change.SessionID), strings.TrimSpace(change.RunID), kind,
			string(payload), formatOutboxTime(occurredAt), now,
		)
		if err != nil {
			return fmt.Errorf("append hub work event: %w", err)
		}
		return nil
	})
}

func (s *Service) commitOutbox(ctx context.Context, record outboxRecord, apply func(*sql.Tx, string) error) (result OutboxItem, resultErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return OutboxItem{}, fmt.Errorf("begin hub state and outbox transaction: %w", err)
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, tx.Rollback())
		}
	}()

	existing, found, err := findOutboxByKey(ctx, tx, record.idempotencyKey)
	if err != nil {
		return OutboxItem{}, err
	}
	if found {
		if !existing.matches(record) {
			return OutboxItem{}, ErrIdempotencyConflict
		}
		if err := tx.Commit(); err != nil {
			return OutboxItem{}, fmt.Errorf("commit idempotent hub mutation lookup: %w", err)
		}
		return existing, nil
	}

	nowTime := s.config.now().UTC()
	now := formatOutboxTime(nowTime)
	if record.coalesceKey != "" {
		if _, err := tx.ExecContext(ctx, `
			UPDATE github_outbox
			SET status = ?, completed_at = ?, updated_at = ?
			WHERE coalesce_key = ? AND status IN (?, ?)`,
			outboxSuperseded, now, now, record.coalesceKey, outboxPending, outboxRetrying,
		); err != nil {
			return OutboxItem{}, fmt.Errorf("coalesce github outbox mutation: %w", err)
		}
	}
	inserted, err := tx.ExecContext(ctx, `
		INSERT INTO github_outbox (
			idempotency_key, repository_id, issue_id, mutation_kind,
			target_node_id, desired_json, status, coalesce_key,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, NULLIF(?, ''), ?, ?, NULLIF(?, ''), ?, ?)`,
		record.idempotencyKey, record.repositoryID, record.issueID, record.kind,
		record.targetNodeID, string(record.desired), outboxPending, record.coalesceKey, now, now,
	)
	if err != nil {
		return OutboxItem{}, fmt.Errorf("enqueue github outbox mutation: %w", err)
	}
	id, err := inserted.LastInsertId()
	if err != nil {
		return OutboxItem{}, fmt.Errorf("read github outbox mutation id: %w", err)
	}
	if err := apply(tx, now); err != nil {
		return OutboxItem{}, err
	}
	if err := tx.Commit(); err != nil {
		return OutboxItem{}, fmt.Errorf("commit hub state and outbox transaction: %w", err)
	}

	result = OutboxItem{
		ID:             id,
		IdempotencyKey: record.idempotencyKey,
		RepositoryID:   record.repositoryID,
		IssueID:        record.issueID,
		Kind:           record.kind,
		TargetNodeID:   record.targetNodeID,
		Desired:        append(json.RawMessage(nil), record.desired...),
		Status:         outboxPending,
		CreatedAt:      nowTime,
	}
	if s.outbox != nil {
		s.outbox.signal()
	}
	return result, nil
}

func findOutboxByKey(ctx context.Context, tx *sql.Tx, key string) (OutboxItem, bool, error) {
	var item OutboxItem
	var desired string
	var createdAt string
	err := tx.QueryRowContext(ctx, `
		SELECT id, idempotency_key, repository_id, COALESCE(issue_id, 0),
		       mutation_kind, COALESCE(target_node_id, ''), desired_json,
		       status, attempts, created_at
		FROM github_outbox
		WHERE idempotency_key = ?`, key,
	).Scan(
		&item.ID, &item.IdempotencyKey, &item.RepositoryID, &item.IssueID,
		&item.Kind, &item.TargetNodeID, &desired, &item.Status, &item.Attempts, &createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return OutboxItem{}, false, nil
	}
	if err != nil {
		return OutboxItem{}, false, fmt.Errorf("find github outbox idempotency key: %w", err)
	}
	item.Desired = json.RawMessage(desired)
	item.CreatedAt, err = parseOutboxTime(createdAt)
	if err != nil {
		return OutboxItem{}, false, fmt.Errorf("parse github outbox creation time: %w", err)
	}
	return item, true, nil
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
	if s.config.OutboxBackend == nil {
		return false, ErrOutboxDisabled
	}
	if ctx == nil {
		ctx = context.Background()
	}
	item, found, err := s.claimOutbox(ctx)
	if err != nil || !found {
		return false, err
	}

	if item.Kind.irreversible() {
		err = s.config.OutboxBackend.VerifyAndExecute(ctx, item)
	} else {
		err = s.config.OutboxBackend.Execute(ctx, item)
	}
	finishCtx, cancel := context.WithTimeout(context.Background(), s.config.BusyTimeout)
	defer cancel()
	if err == nil {
		if finishErr := s.completeOutbox(finishCtx, item); finishErr != nil {
			return true, finishErr
		}
		return true, nil
	}
	if finishErr := s.failOutbox(finishCtx, item, err); finishErr != nil {
		return true, errors.Join(err, finishErr)
	}
	return true, err
}

func (s *Service) claimOutbox(ctx context.Context) (result OutboxItem, found bool, resultErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return OutboxItem{}, false, fmt.Errorf("begin github outbox claim: %w", err)
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, tx.Rollback())
		}
	}()

	now := s.config.now()
	staleBefore := formatOutboxTime(now.Add(-s.config.OutboxProcessingTimeout))
	var desired string
	var createdAt string
	err = tx.QueryRowContext(ctx, `
		SELECT o.id, o.idempotency_key, o.repository_id, r.github_owner, r.github_name,
		       COALESCE(o.issue_id, 0), COALESCE(i.github_number, 0), o.mutation_kind,
		       COALESCE(o.target_node_id, ''), o.desired_json, o.status, o.attempts, o.created_at
		FROM github_outbox o
		JOIN repositories r ON r.id = o.repository_id
		LEFT JOIN issues i ON i.id = o.issue_id
		WHERE (
			o.status IN (?, ?)
			AND (o.next_retry_at IS NULL OR o.next_retry_at <= ?)
		) OR (
			o.status = ? AND o.processing_started_at <= ?
		)
		ORDER BY o.id
		LIMIT 1`,
		outboxPending, outboxRetrying, formatOutboxTime(now), outboxProcessing, staleBefore,
	).Scan(
		&result.ID, &result.IdempotencyKey, &result.RepositoryID,
		&result.RepositoryOwner, &result.RepositoryName, &result.IssueID,
		&result.IssueNumber, &result.Kind, &result.TargetNodeID, &desired,
		&result.Status, &result.Attempts, &createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return OutboxItem{}, false, fmt.Errorf("commit empty github outbox claim: %w", err)
		}
		return OutboxItem{}, false, nil
	}
	if err != nil {
		return OutboxItem{}, false, fmt.Errorf("select github outbox mutation: %w", err)
	}

	result.Attempts++
	result.Status = outboxProcessing
	result.Desired = json.RawMessage(desired)
	result.CreatedAt, err = parseOutboxTime(createdAt)
	if err != nil {
		return OutboxItem{}, false, fmt.Errorf("parse claimed github outbox creation time: %w", err)
	}
	update, err := tx.ExecContext(ctx, `
		UPDATE github_outbox
		SET status = ?, attempts = ?, last_attempt_at = ?, processing_started_at = ?,
		    next_retry_at = NULL, updated_at = ?
		WHERE id = ?`,
		outboxProcessing, result.Attempts, formatOutboxTime(now), formatOutboxTime(now),
		formatOutboxTime(now), result.ID,
	)
	if err != nil {
		return OutboxItem{}, false, fmt.Errorf("claim github outbox mutation: %w", err)
	}
	rows, err := update.RowsAffected()
	if err != nil || rows != 1 {
		return OutboxItem{}, false, fmt.Errorf("claim github outbox mutation rows = %d: %w", rows, err)
	}
	if err := tx.Commit(); err != nil {
		return OutboxItem{}, false, fmt.Errorf("commit github outbox claim: %w", err)
	}
	return result, true, nil
}

func (s *Service) completeOutbox(ctx context.Context, item OutboxItem) error {
	now := formatOutboxTime(s.config.now())
	result, err := s.database.db.ExecContext(ctx, `
		UPDATE github_outbox
		SET status = ?, completed_at = ?, processing_started_at = NULL,
		    terminal_error = NULL, operator_action = NULL, updated_at = ?
		WHERE id = ? AND status = ? AND attempts = ?`,
		outboxCompleted, now, now, item.ID, outboxProcessing, item.Attempts,
	)
	if err != nil {
		return fmt.Errorf("complete github outbox mutation: %w", err)
	}
	return requireOneOutboxRow(result, "complete")
}

func (s *Service) failOutbox(ctx context.Context, item OutboxItem, executionErr error) error {
	now := s.config.now()
	message := truncateOutboxError(executionErr.Error())
	if IsPermanent(executionErr) || item.Attempts >= s.config.OutboxMaxAttempts {
		action := "Inspect the failed GitHub mutation, correct its target or credentials, and enqueue a new mutation with a new idempotency key."
		result, err := s.database.db.ExecContext(ctx, `
			UPDATE github_outbox
			SET status = ?, terminal_error = ?, operator_action = ?,
			    processing_started_at = NULL, next_retry_at = NULL, updated_at = ?
			WHERE id = ? AND status = ? AND attempts = ?`,
			outboxDeadLetter, message, action, formatOutboxTime(now),
			item.ID, outboxProcessing, item.Attempts,
		)
		if err != nil {
			return fmt.Errorf("dead-letter github outbox mutation: %w", err)
		}
		return requireOneOutboxRow(result, "dead-letter")
	}

	nextRetry := now.Add(s.outboxBackoff(item.Attempts))
	result, err := s.database.db.ExecContext(ctx, `
		UPDATE github_outbox
		SET status = ?, next_retry_at = ?, processing_started_at = NULL,
		    terminal_error = ?, updated_at = ?
		WHERE id = ? AND status = ? AND attempts = ?`,
		outboxRetrying, formatOutboxTime(nextRetry), message, formatOutboxTime(now),
		item.ID, outboxProcessing, item.Attempts,
	)
	if err != nil {
		return fmt.Errorf("retry github outbox mutation: %w", err)
	}
	return requireOneOutboxRow(result, "retry")
}

func requireOneOutboxRow(result sql.Result, operation string) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read %s github outbox result: %w", operation, err)
	}
	if rows != 1 {
		return fmt.Errorf("%s github outbox mutation changed %d rows, want 1", operation, rows)
	}
	return nil
}

func (s *Service) outboxBackoff(attempt int) time.Duration {
	exponent := max(attempt-1, 0)
	multiplier := math.Pow(2, float64(min(exponent, 62)))
	delay := time.Duration(float64(s.config.OutboxBaseBackoff) * multiplier)
	if delay <= 0 || delay > s.config.OutboxMaxBackoff {
		delay = s.config.OutboxMaxBackoff
	}
	factor := 0.5 + s.config.jitter()
	if factor < 0.5 {
		factor = 0.5
	}
	if factor > 1.5 {
		factor = 1.5
	}
	delay = time.Duration(float64(delay) * factor)
	if delay > s.config.OutboxMaxBackoff {
		return s.config.OutboxMaxBackoff
	}
	return delay
}

func randomJitter() float64 {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return 0.5
	}
	return float64(binary.LittleEndian.Uint64(value[:])>>11) / (1 << 53)
}

func (s *Service) OutboxHealth(ctx context.Context) (OutboxHealth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var health OutboxHealth
	if err := s.readOutboxStatusCounts(ctx, &health); err != nil {
		return OutboxHealth{}, err
	}

	var oldest sql.NullString
	if err := s.database.db.QueryRowContext(ctx, `
		SELECT MIN(created_at) FROM github_outbox WHERE status IN (?, ?, ?)`,
		outboxPending, outboxRetrying, outboxProcessing,
	).Scan(&oldest); err != nil {
		return OutboxHealth{}, fmt.Errorf("query oldest github outbox mutation: %w", err)
	}
	if oldest.Valid {
		parsed, err := parseOutboxTime(oldest.String)
		if err != nil {
			return OutboxHealth{}, fmt.Errorf("parse oldest github outbox mutation: %w", err)
		}
		health.OldestPendingAt = &parsed
	}

	actions, err := s.database.db.QueryContext(ctx, `
		SELECT id, idempotency_key, mutation_kind, COALESCE(target_node_id, ''), operator_action
		FROM github_outbox
		WHERE status = ? AND operator_action IS NOT NULL
		ORDER BY id`, outboxDeadLetter,
	)
	if err != nil {
		return OutboxHealth{}, fmt.Errorf("query github outbox operator actions: %w", err)
	}
	defer actions.Close()
	for actions.Next() {
		var action OperatorAction
		if err := actions.Scan(&action.ID, &action.IdempotencyKey, &action.Kind, &action.TargetNodeID, &action.Action); err != nil {
			return OutboxHealth{}, fmt.Errorf("scan github outbox operator action: %w", err)
		}
		health.OperatorActions = append(health.OperatorActions, action)
	}
	if err := actions.Err(); err != nil {
		return OutboxHealth{}, fmt.Errorf("iterate github outbox operator actions: %w", err)
	}
	return health, nil
}

func (s *Service) readOutboxStatusCounts(ctx context.Context, health *OutboxHealth) (resultErr error) {
	rows, err := s.database.db.QueryContext(ctx, `
		SELECT status, COUNT(*)
		FROM github_outbox
		WHERE status IN (?, ?, ?, ?)
		GROUP BY status`,
		outboxPending, outboxRetrying, outboxProcessing, outboxDeadLetter,
	)
	if err != nil {
		return fmt.Errorf("query github outbox health: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, rows.Close())
	}()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return fmt.Errorf("scan github outbox health: %w", err)
		}
		switch status {
		case outboxPending:
			health.Pending = count
		case outboxRetrying:
			health.Retrying = count
		case outboxProcessing:
			health.Processing = count
		case outboxDeadLetter:
			health.DeadLetters = count
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate github outbox health: %w", err)
	}
	return nil
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
			processed, err := w.service.ProcessOutbox(ctx)
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
