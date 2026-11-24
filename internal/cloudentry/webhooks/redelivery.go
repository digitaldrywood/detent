package webhooks

import (
	"context"
	"log/slog"
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

func RequestRedeliveries(ctx context.Context, app github.InstallationTokenConfig, logger *slog.Logger, since, before time.Time) (int, error) {
	token, err := github.NewAppTokenSource(app)
	if err != nil {
		return 0, err
	}
	client, err := github.NewClient(github.ClientConfig{Endpoint: app.Endpoint, TokenSource: token, HTTPClient: app.HTTPClient, DisableConditionalRequests: true, Logger: logger})
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
