package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var _ accountstore.SummaryDeliveryRepository = (*Repository)(nil)

func (r *Repository) DueSummaries(ctx context.Context, org tracker.OrganizationID, now time.Time) ([]accountstore.SummaryRecipient, error) {
	out := []accountstore.SummaryRecipient{}
	err := r.transaction(ctx, org, corestore.Background, true, func(s *session) error {
		rows, err := s.queries.DueSummaries(ctx, pgquery.DueSummariesParams{OrganizationID: org, DueAt: now, NowAt: now})
		if err != nil {
			return mapError(ctx, err)
		}
		for _, row := range rows {
			out = append(out, accountstore.SummaryRecipient{User: row.UserID, Organization: string(row.OrganizationID), Email: row.Email, Principal: row.PrincipalID, Role: row.Role, Token: row.Token, TimeZone: row.TimeZone, Eligible: row.Eligible, Weekdays: row.Weekdays, Minute: int(row.SendMinute), End: row.WindowEnd.UTC()})
		}
		return nil
	})
	return out, err
}

func (r *Repository) SummaryStatus(ctx context.Context, org tracker.OrganizationID, user string, end time.Time) (string, error) {
	var status string
	err := r.transaction(ctx, org, corestore.Background, true, func(s *session) error {
		var err error
		status, err = s.queries.SummaryStatus(ctx, pgquery.SummaryStatusParams{OrganizationID: org, UserID: user, WindowEnd: end})
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return mapError(ctx, err)
	})
	return status, err
}

func (r *Repository) ClaimSummary(ctx context.Context, org tracker.OrganizationID, receipt accountstore.SummaryReceipt) (bool, error) {
	if receipt.Status != "uncertain" && receipt.Status != "quiet" {
		return false, errors.New("invalid summary receipt status")
	}
	var claimed bool
	err := r.transaction(ctx, org, corestore.Background, false, func(s *session) error {
		count, err := s.queries.ClaimSummary(ctx, pgquery.ClaimSummaryParams{OrganizationID: org, UserID: receipt.User, WindowEnd: receipt.End, Status: receipt.Status, RecordedAt: receipt.RecordedAt})
		claimed = count == 1
		return mapError(ctx, err)
	})
	return claimed, err
}

func (r *Repository) FinishSummary(ctx context.Context, org tracker.OrganizationID, receipt accountstore.SummaryReceipt) error {
	if receipt.Status != "sent" && receipt.Status != "rejected" && receipt.Status != "uncertain" {
		return errors.New("invalid summary receipt status")
	}
	return r.transaction(ctx, org, corestore.Background, false, func(s *session) error {
		if err := s.queries.FinishSummary(ctx, pgquery.FinishSummaryParams{OrganizationID: org, UserID: receipt.User, WindowEnd: receipt.End, Status: receipt.Status, RecordedAt: receipt.RecordedAt}); err != nil {
			return mapError(ctx, err)
		}
		if receipt.Status == "rejected" {
			return mapError(ctx, s.queries.RetrySummary(ctx, pgquery.RetrySummaryParams{OrganizationID: org, UserID: receipt.User, WindowEnd: receipt.End, NowAt: receipt.RecordedAt, Jitter: int64(receipt.RetryJitter / time.Second)}))
		}
		return s.advanceSummary(ctx, receipt)
	})
}

func (r *Repository) AdvanceSummary(ctx context.Context, org tracker.OrganizationID, receipt accountstore.SummaryReceipt) error {
	return r.transaction(ctx, org, corestore.Background, false, func(s *session) error { return s.advanceSummary(ctx, receipt) })
}

func (s *session) advanceSummary(ctx context.Context, receipt accountstore.SummaryReceipt) error {
	return mapError(ctx, s.queries.AdvanceSummary(ctx, pgquery.AdvanceSummaryParams{OrganizationID: s.organization, UserID: receipt.User, WindowEnd: receipt.End, WindowEnd_2: receipt.NextEnd, DueAt: receipt.NextDue}))
}
