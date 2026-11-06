package hubserver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/emailtemplate"
	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) deliverDailySummaries(ctx context.Context, now time.Time) error {
	if s.config.Hosted == nil || s.config.Hosted.EmailSender == nil {
		return nil
	}
	repository, ok := s.config.AccessRepository.(accountstore.SummaryDeliveryRepository)
	if !ok {
		return errors.New("summary delivery repository unavailable")
	}
	rows, err := repository.DueSummaries(ctx, s.accessOrganization(), now)
	if err != nil {
		return fmt.Errorf("read due summaries: %w", err)
	}
	recipients := make([]summaryRecipient, 0, len(rows))
	for _, row := range rows {
		zone, err := time.LoadLocation(row.TimeZone)
		if err != nil {
			return fmt.Errorf("load summary time zone: %w", err)
		}
		recipients = append(recipients, summaryRecipient{user: row.User, organization: row.Organization, email: row.Email, principal: row.Principal, role: row.Role, token: row.Token, eligible: row.Eligible, minute: row.Minute, zone: zone, weekdays: row.Weekdays, end: row.End})
	}
	var failures error
	for _, r := range recipients {
		if !r.eligible {
			if err := s.advanceDailySummary(ctx, r, now); err != nil {
				failures = errors.Join(failures, err)
			}
			continue
		}
		if err := s.deliverDailySummary(ctx, r, now); err != nil {
			failures = errors.Join(failures, err)
		}
	}
	return failures
}

func (s *Service) deliverDailySummary(ctx context.Context, r summaryRecipient, now time.Time) error {
	repository, ok := s.config.AccessRepository.(accountstore.SummaryDeliveryRepository)
	if !ok {
		return errors.New("summary delivery repository unavailable")
	}
	status, err := repository.SummaryStatus(ctx, tracker.OrganizationID(r.organization), r.user, r.end)
	if err != nil {
		return err
	}
	if status != "" && status != "rejected" {
		return s.advanceDailySummary(ctx, r, now)
	}
	data, quiet, err := s.buildDailySummary(ctx, r)
	if err != nil {
		return fmt.Errorf("build daily summary: %w", err)
	}
	var message auth.EmailMessage
	if !quiet {
		message, err = emailtemplate.RenderDailySummary(data)
		if err != nil {
			return err
		}
		message.To = r.email
	}
	receipt := summaryDeliveryReceipt(r, now)
	receipt.Status = "uncertain"
	if quiet {
		receipt.Status = "quiet"
	}
	claimed, err := repository.ClaimSummary(ctx, tracker.OrganizationID(r.organization), receipt)
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	if quiet {
		return s.advanceDailySummary(ctx, r, now)
	}
	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	sendErr := s.config.Hosted.EmailSender.SendEmail(sendCtx, message)
	cancel()
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer finishCancel()
	outcome := auth.EmailDeliveryOutcome(sendErr)
	status = "uncertain"
	if outcome == "committed" {
		status = "sent"
	}
	if outcome == "rejected" {
		status = "rejected"
	}
	receipt.Status = status
	if err := repository.FinishSummary(finishCtx, tracker.OrganizationID(r.organization), receipt); err != nil {
		return errors.Join(sendErr, err)
	}
	if sendErr != nil {
		return fmt.Errorf("send daily summary: %w", sendErr)
	}
	return nil
}

func summaryDeliveryReceipt(r summaryRecipient, now time.Time) accountstore.SummaryReceipt {
	next := summarySchedule(maxTime(r.end, now), r.minute, r.zone, r.weekdays, 1)
	return accountstore.SummaryReceipt{User: r.user, End: r.end, RecordedAt: now, NextEnd: next, NextDue: next.Add(summaryJitter(r.user, r.organization, next)), RetryJitter: summaryJitter(r.user, r.organization, now)}
}

func (s *Service) advanceDailySummary(ctx context.Context, r summaryRecipient, now time.Time) error {
	repository, ok := s.config.AccessRepository.(accountstore.SummaryDeliveryRepository)
	if !ok {
		return errors.New("summary delivery repository unavailable")
	}
	return repository.AdvanceSummary(ctx, tracker.OrganizationID(r.organization), summaryDeliveryReceipt(r, now))
}
