package cloudentry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/connector/github"
)

type githubDelivery struct {
	ID          int64     `json:"id"`
	GUID        string    `json:"guid"`
	DeliveredAt time.Time `json:"delivered_at"`
	StatusCode  int       `json:"status_code"`
	Event       string    `json:"event"`
}

func (s *Service) githubReplayWindow(ctx context.Context) (time.Time, time.Time) {
	before := s.config.now()
	if s.config.GitHubApp == nil || len(s.config.GitHubWebhookSecret) == 0 {
		return time.Time{}, before
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
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
		count, err = s.requestGitHubRedeliveries(ctx, since, before)
	}
	if err != nil {
		s.config.Logger.Warn("replay GitHub App deliveries", "requested", count, "since", since, "before", before, "error", err)
		return
	}
	s.config.Logger.Info("replay GitHub App deliveries", "requested", count, "since", since, "before", before)
}

func (s *Service) requestGitHubRedeliveries(ctx context.Context, since, before time.Time) (int, error) {
	token, err := github.NewAppTokenSource(*s.config.GitHubApp)
	if err != nil {
		return 0, err
	}
	client, err := github.NewClient(github.ClientConfig{Endpoint: s.config.GitHubApp.Endpoint, TokenSource: token, HTTPClient: s.config.GitHubApp.HTTPClient, DisableConditionalRequests: true, Logger: s.config.Logger})
	if err != nil {
		return 0, err
	}
	latest := make(map[string]githubDelivery)
	for path := "/app/hook/deliveries?per_page=100"; path != ""; {
		var page []githubDelivery
		path, err = client.RESTPage(ctx, path, &page)
		if err != nil {
			return 0, err
		}
		pastWindow := false
		for _, delivery := range page {
			if delivery.DeliveredAt.Before(since) {
				pastWindow = true
				continue
			}
			if !delivery.DeliveredAt.Before(before) || delivery.GUID == "" || delivery.ID <= 0 {
				continue
			}
			previous, ok := latest[delivery.GUID]
			if !ok || delivery.DeliveredAt.After(previous.DeliveredAt) || delivery.DeliveredAt.Equal(previous.DeliveredAt) && delivery.ID > previous.ID {
				latest[delivery.GUID] = delivery
			}
		}
		if pastWindow {
			break
		}
	}
	deliveries := make([]int64, 0, len(latest))
	for _, delivery := range latest {
		if (delivery.Event == "issues" || delivery.Event == "issue_comment") && (delivery.StatusCode < 200 || delivery.StatusCode >= 300) {
			deliveries = append(deliveries, delivery.ID)
		}
	}
	slices.Sort(deliveries)
	count := 0
	for _, id := range deliveries {
		if err := client.REST(ctx, http.MethodPost, "/app/hook/deliveries/"+strconv.FormatInt(id, 10)+"/attempts", nil, nil); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}
