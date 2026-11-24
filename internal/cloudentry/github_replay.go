package cloudentry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudentry/webhooks"
)

func (s *Service) githubReplayWindow(ctx context.Context) (time.Time, time.Time) {
	before := s.config.Now()
	if s.config.GitHubApp == nil || len(s.config.GitHubWebhookSecret) == 0 {
		return time.Time{}, before
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if s.sharedHub != nil {
		latest, err := s.registry.store.db.postgres.LatestGitHubIngress(ctx)
		if err != nil {
			s.config.Logger.Warn("read GitHub ingress replay window", "error", err)
			return time.Time{}, before
		}
		if !latest.IsZero() {
			return latest, before
		}
		var since *time.Time
		err = s.registry.store.db.QueryRowContext(ctx, `SELECT (
SELECT COALESCE(r.last_received_at,r.received_at)
FROM public.github_webhook_receipts r JOIN platform.organizations o ON o.id=r.organization_id
WHERE o.state='ready' ORDER BY COALESCE(r.last_received_at,r.received_at) DESC LIMIT 1)`).Scan(&since)
		if err != nil {
			s.config.Logger.Warn("read GitHub replay window", "error", err)
		}
		if since == nil {
			return time.Time{}, before
		}
		return *since, before
	}
	organizations, err := s.registry.List(ctx)
	if err != nil {
		s.config.Logger.Warn("read GitHub replay window", "error", err)
		return time.Time{}, before
	}
	var since time.Time
	for _, organization := range organizations {
		if organization.State != "ready" {
			continue
		}
		status, raw, err := s.serviceCall(ctx, organization, "/internal/v1/github/receipt", struct{}{}, nil)
		if err == nil && status != http.StatusOK {
			err = fmt.Errorf("tenant GitHub receipt returned %d", status)
		}
		var receipt struct {
			ReceivedAt *time.Time `json:"received_at"`
		}
		if err == nil {
			err = json.Unmarshal(raw, &receipt)
		}
		if err != nil {
			s.config.Logger.Warn("read GitHub replay window", "organization", organization.ID, "error", err)
			return time.Time{}, before
		}
		if receipt.ReceivedAt != nil && receipt.ReceivedAt.After(since) {
			since = *receipt.ReceivedAt
		}
	}
	return since, before
}

func (s *Service) replayGitHubDeliveries(ctx context.Context, since, before time.Time) {
	if s.config.GitHubApp == nil || len(s.config.GitHubWebhookSecret) == 0 {
		return
	}
	var count int
	var err error
	if !since.IsZero() && since.Before(before) {
		count, err = webhooks.RequestRedeliveries(ctx, *s.config.GitHubApp, s.config.Logger, since, before)
	}
	if err != nil {
		s.config.Logger.Warn("replay GitHub App deliveries", "requested", count, "since", since, "before", before, "error", err)
		return
	}
	s.config.Logger.Info("replay GitHub App deliveries", "requested", count, "since", since, "before", before)
}
