package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type OperatorReceiptReservation struct {
	Receipt       receiptstore.Record
	ProjectID     tracker.ProjectID
	CorrelationID string
}

type OperatorReceiptState struct {
	Receipt       receiptstore.Record
	ProjectID     tracker.ProjectID
	CorrelationID string
	ClaimedAt     *time.Time
}

type OperatorReceiptReader interface {
	OperatorReceipt(context.Context, tracker.ProjectID, receiptstore.Record) (OperatorReceiptState, error)
}

type OperatorReceiptWriter interface {
	OperatorReceiptReader
	ReserveOperatorReceipt(context.Context, OperatorReceiptReservation) (OperatorReceiptState, bool, error)
	ClaimOperatorReceipt(context.Context, OperatorReceiptReservation) (OperatorReceiptState, bool, error)
	CompleteOperatorReceipt(context.Context, OperatorReceiptReservation, bool) (OperatorReceiptState, error)
}

func (s *session) checkOperatorReceipt(record receiptstore.Record) error {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return err
	}
	return s.checkReceipt(receiptstore.Record{OrganizationID: record.OrganizationID, ActorID: record.ActorID, Operation: record.Operation, CommandKey: record.CommandKey, RequestHash: record.RequestHash, ResponseClaimed: true, ResponsePending: true})
}

func (s *session) operatorReceiptCustody(ctx context.Context, record receiptstore.Record) (pgquery.OperatorReceiptCustodyRow, error) {
	row, err := s.queries.OperatorReceiptCustody(ctx, pgquery.OperatorReceiptCustodyParams{OrganizationID: s.organization, ActorID: record.ActorID, Operation: record.Operation, CommandKey: record.CommandKey})
	err = mapError(ctx, err)
	if errors.Is(err, corestore.ErrNotFound) {
		err = nil
	}
	return row, err
}

func (s *session) OperatorReceipt(ctx context.Context, project tracker.ProjectID, expected receiptstore.Record) (OperatorReceiptState, error) {
	if err := s.checkOperatorReceipt(expected); err != nil {
		return OperatorReceiptState{}, err
	}
	record, err := s.Receipt(ctx, expected.ActorID, expected.Operation, expected.CommandKey)
	if err != nil {
		return OperatorReceiptState{}, err
	}
	custody, err := s.operatorReceiptCustody(ctx, record)
	if err != nil {
		return OperatorReceiptState{}, err
	}
	if custody.CorrelationID == "" || custody.ProjectID != string(project) {
		return OperatorReceiptState{}, corestore.ErrNotFound
	}
	if record.RequestHash != expected.RequestHash {
		return OperatorReceiptState{}, mutation.ErrConflict
	}
	return OperatorReceiptState{Receipt: record, ProjectID: project, CorrelationID: custody.CorrelationID, ClaimedAt: custody.ClaimedAt}, nil
}

func (s *session) checkOperatorReservation(reservation OperatorReceiptReservation) error {
	if reservation.CorrelationID == "" || len(reservation.CorrelationID) > 256 {
		return eventstore.ErrInvalidRecord
	}
	return s.checkOperatorReceipt(reservation.Receipt)
}

func (s *session) ReserveOperatorReceipt(ctx context.Context, reservation OperatorReceiptReservation) (OperatorReceiptState, bool, error) {
	if err := s.checkOperatorReservation(reservation); err != nil {
		return OperatorReceiptState{}, false, err
	}
	if reservation.ProjectID != "" {
		if err := s.LockWorkItemProject(ctx, reservation.ProjectID); err != nil {
			return OperatorReceiptState{}, false, err
		}
	}
	record := reservation.Receipt
	inserted, err := s.queries.ReserveOperatorReceipt(ctx, pgquery.ReserveOperatorReceiptParams{OrganizationID: s.organization, ActorID: record.ActorID, Operation: record.Operation, CommandKey: record.CommandKey, RequestHash: record.RequestHash, CreatedAt: recordTime(record.CreatedAt), ReservationProjectID: new(string(reservation.ProjectID)), ReservationCorrelationID: new(reservation.CorrelationID)})
	if err != nil {
		return OperatorReceiptState{}, false, mapError(ctx, err)
	}
	state, err := s.OperatorReceipt(ctx, reservation.ProjectID, record)
	return state, inserted == 1, err
}

func (s *session) ClaimOperatorReceipt(ctx context.Context, reservation OperatorReceiptReservation) (OperatorReceiptState, bool, error) {
	if err := s.checkOperatorReservation(reservation); err != nil {
		return OperatorReceiptState{}, false, err
	}
	record := reservation.Receipt
	if _, err := s.lockReceipt(ctx, record.ActorID, record.Operation, record.CommandKey); err != nil {
		return OperatorReceiptState{}, false, err
	}
	state, err := s.OperatorReceipt(ctx, reservation.ProjectID, record)
	if err != nil {
		return OperatorReceiptState{}, false, err
	}
	if state.CorrelationID != reservation.CorrelationID || state.ClaimedAt != nil || !state.Receipt.ResponseClaimed || state.Receipt.ResponseExpired {
		return state, false, nil
	}
	claimed, err := s.queries.ClaimReservedOperatorReceipt(ctx, pgquery.ClaimReservedOperatorReceiptParams{OrganizationID: s.organization, ActorID: record.ActorID, Operation: record.Operation, CommandKey: record.CommandKey, RequestHash: record.RequestHash, ProjectID: string(reservation.ProjectID), CorrelationID: reservation.CorrelationID, ClaimedAt: recordTime(time.Time{})})
	if err != nil {
		return OperatorReceiptState{}, false, mapError(ctx, err)
	}
	state, err = s.OperatorReceipt(ctx, reservation.ProjectID, record)
	return state, claimed == 1, err
}

func (s *session) CompleteOperatorReceipt(ctx context.Context, reservation OperatorReceiptReservation, definitelyRejected bool) (OperatorReceiptState, error) {
	if err := s.checkOperatorReservation(reservation); err != nil {
		return OperatorReceiptState{}, err
	}
	record := reservation.Receipt
	if record.ResponseClaimed || record.ResponsePending || record.ResponseExpired || record.ResponseRequestID != "" {
		return OperatorReceiptState{}, eventstore.ErrInvalidRecord
	}
	if _, err := s.lockReceipt(ctx, record.ActorID, record.Operation, record.CommandKey); err != nil {
		return OperatorReceiptState{}, err
	}
	state, err := s.OperatorReceipt(ctx, reservation.ProjectID, record)
	if err != nil {
		return OperatorReceiptState{}, err
	}
	if state.CorrelationID != reservation.CorrelationID || state.ClaimedAt == nil && !definitelyRejected && state.Receipt.ResponseClaimed {
		return OperatorReceiptState{}, mutation.ErrUncertain
	}
	if _, err := s.completeReceipt(ctx, record, reservation.CorrelationID); err != nil {
		return OperatorReceiptState{}, err
	}
	retained, err := s.queries.RetainOperatorReceiptReplay(ctx, pgquery.RetainOperatorReceiptReplayParams{OrganizationID: s.organization, ActorID: record.ActorID, Operation: record.Operation, CommandKey: record.CommandKey, RequestHash: record.RequestHash, ReservationProjectID: new(string(reservation.ProjectID)), ReservationCorrelationID: new(reservation.CorrelationID)})
	if err != nil {
		return OperatorReceiptState{}, mapError(ctx, err)
	}
	if retained != 1 {
		return OperatorReceiptState{}, mutation.ErrUncertain
	}
	return s.OperatorReceipt(ctx, reservation.ProjectID, record)
}
