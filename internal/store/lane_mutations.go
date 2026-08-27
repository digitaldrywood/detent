package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/store/sqlc"
)

func (s *sqliteStore) BeginLaneMutation(ctx context.Context, attrs LaneMutationStart) (LaneMutationReceipt, error) {
	if err := validateLaneMutationStart(attrs); err != nil {
		return LaneMutationReceipt{}, err
	}
	requestedAt, err := requiredTimestamp("requested_at", attrs.RequestedAt)
	if err != nil {
		return LaneMutationReceipt{}, err
	}
	receipt, err := s.queries.CreateLaneMutationReceipt(ctx, sqlc.CreateLaneMutationReceiptParams{
		ProjectID:     strings.TrimSpace(attrs.ProjectID),
		IssueID:       strings.TrimSpace(attrs.IssueID),
		Generation:    int64(attrs.Generation),
		Disposition:   string(attrs.Disposition),
		FromState:     strings.TrimSpace(attrs.FromState),
		ToState:       strings.TrimSpace(attrs.ToState),
		Reason:        strings.TrimSpace(attrs.Reason),
		RequestedAt:   requestedAt,
		WorkAttemptID: attrs.WorkAttemptID,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return LaneMutationReceipt{}, ErrNotFound
		}
		return LaneMutationReceipt{}, fmt.Errorf("beginning lane mutation: %w", err)
	}
	return laneMutationReceiptFromRow(receipt)
}

func (s *sqliteStore) ResolveLaneMutation(ctx context.Context, attrs LaneMutationResolution) error {
	if attrs.ReceiptID <= 0 || attrs.WorkAttemptID <= 0 || attrs.Generation == 0 || attrs.Generation > math.MaxInt64 {
		return errors.New("lane mutation owner is required")
	}
	switch attrs.TrackerResult {
	case LaneMutationTrackerApplied, LaneMutationTrackerBlocked, LaneMutationTrackerFailed:
	default:
		return fmt.Errorf("invalid lane mutation tracker result %q", attrs.TrackerResult)
	}
	resolvedAt, err := requiredTimestamp("resolved_at", attrs.ResolvedAt)
	if err != nil {
		return err
	}
	rows, err := s.queries.ResolveLaneMutationReceipt(ctx, sqlc.ResolveLaneMutationReceiptParams{
		TrackerResult: string(attrs.TrackerResult),
		ResolvedAt:    sql.NullString{String: resolvedAt, Valid: true},
		ErrorMessage:  strings.TrimSpace(attrs.ErrorMessage),
		ID:            attrs.ReceiptID,
		WorkAttemptID: attrs.WorkAttemptID,
		Generation:    int64(attrs.Generation),
	})
	if err != nil {
		return fmt.Errorf("resolving lane mutation: %w", err)
	}
	return requireAffected(rows, "lane mutation receipt", attrs.ReceiptID)
}

func (s *sqliteStore) LaneMutationReceipt(ctx context.Context, attrs LaneMutationLookup) (LaneMutationReceipt, error) {
	if err := validateLaneMutationOwner(attrs.ProjectID, attrs.IssueID, attrs.WorkAttemptID, attrs.Generation, attrs.ToState); err != nil {
		return LaneMutationReceipt{}, err
	}
	receipt, err := s.queries.LaneMutationReceiptForOwner(ctx, sqlc.LaneMutationReceiptForOwnerParams{
		ProjectID:     strings.TrimSpace(attrs.ProjectID),
		IssueID:       strings.TrimSpace(attrs.IssueID),
		WorkAttemptID: attrs.WorkAttemptID,
		Generation:    int64(attrs.Generation),
		ToState:       strings.TrimSpace(attrs.ToState),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return LaneMutationReceipt{}, ErrNotFound
		}
		return LaneMutationReceipt{}, fmt.Errorf("loading lane mutation receipt: %w", err)
	}
	return laneMutationReceiptFromRow(receipt)
}

func (s *sqliteStore) ConsumeLaneMutation(ctx context.Context, attrs LaneMutationConsumption) (LaneMutationReceipt, error) {
	if attrs.ReceiptID <= 0 {
		return LaneMutationReceipt{}, errors.New("lane mutation receipt ID is required")
	}
	if err := validateLaneMutationOwner(attrs.ProjectID, attrs.IssueID, attrs.WorkAttemptID, attrs.Generation, attrs.ToState); err != nil {
		return LaneMutationReceipt{}, err
	}
	consumedAt, err := requiredTimestamp("consumed_at", attrs.ConsumedAt)
	if err != nil {
		return LaneMutationReceipt{}, err
	}
	receipt, err := s.queries.ConsumeLaneMutationReceipt(ctx, sqlc.ConsumeLaneMutationReceiptParams{
		ConsumedAt:    sql.NullString{String: consumedAt, Valid: true},
		ID:            attrs.ReceiptID,
		ProjectID:     strings.TrimSpace(attrs.ProjectID),
		IssueID:       strings.TrimSpace(attrs.IssueID),
		WorkAttemptID: attrs.WorkAttemptID,
		Generation:    int64(attrs.Generation),
		ToState:       strings.TrimSpace(attrs.ToState),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return LaneMutationReceipt{}, ErrNotFound
		}
		return LaneMutationReceipt{}, fmt.Errorf("consuming lane mutation: %w", err)
	}
	return laneMutationReceiptFromRow(receipt)
}

func validateLaneMutationOwner(projectID string, issueID string, workAttemptID int64, generation uint64, toState string) error {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(issueID) == "" || workAttemptID <= 0 ||
		generation == 0 || generation > math.MaxInt64 || strings.TrimSpace(toState) == "" {
		return errors.New("lane mutation owner and target state are required")
	}
	return nil
}

func validateLaneMutationStart(attrs LaneMutationStart) error {
	if strings.TrimSpace(attrs.ProjectID) == "" || strings.TrimSpace(attrs.IssueID) == "" || attrs.WorkAttemptID <= 0 ||
		attrs.Generation == 0 || attrs.Generation > math.MaxInt64 {
		return errors.New("lane mutation owner is required")
	}
	if strings.TrimSpace(attrs.ToState) == "" {
		return errors.New("lane mutation target state is required")
	}
	if strings.TrimSpace(attrs.Reason) == "" {
		return errors.New("lane mutation reason is required")
	}
	switch attrs.Disposition {
	case LaneMutationPreserveOwnership, LaneMutationAcceptCompletion, LaneMutationRevokeWorker:
		return nil
	default:
		return fmt.Errorf("invalid lane mutation disposition %q", attrs.Disposition)
	}
}

func laneMutationReceiptFromRow(row sqlc.LaneMutationReceipt) (LaneMutationReceipt, error) {
	requestedAt, err := parseTimestamp("requested_at", row.RequestedAt)
	if err != nil {
		return LaneMutationReceipt{}, err
	}
	var resolvedAt time.Time
	if row.ResolvedAt.Valid {
		resolvedAt, err = parseTimestamp("resolved_at", row.ResolvedAt.String)
		if err != nil {
			return LaneMutationReceipt{}, err
		}
	}
	var consumedAt time.Time
	if row.ConsumedAt.Valid {
		consumedAt, err = parseTimestamp("consumed_at", row.ConsumedAt.String)
		if err != nil {
			return LaneMutationReceipt{}, err
		}
	}
	if row.Generation <= 0 {
		return LaneMutationReceipt{}, errors.New("lane mutation generation is invalid")
	}
	return LaneMutationReceipt{
		ID:            row.ID,
		ProjectID:     strings.TrimSpace(row.ProjectID),
		IssueID:       strings.TrimSpace(row.IssueID),
		WorkAttemptID: row.WorkAttemptID,
		Generation:    uint64(row.Generation),
		Disposition:   LaneMutationDisposition(row.Disposition),
		FromState:     strings.TrimSpace(row.FromState),
		ToState:       strings.TrimSpace(row.ToState),
		Reason:        strings.TrimSpace(row.Reason),
		TrackerResult: LaneMutationTrackerResult(row.TrackerResult),
		RequestedAt:   requestedAt.UTC(),
		ResolvedAt:    resolvedAt.UTC(),
		ConsumedAt:    consumedAt.UTC(),
		ErrorMessage:  row.ErrorMessage.String,
	}, nil
}
