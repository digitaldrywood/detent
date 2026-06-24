package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

const githubWebhookMaxBodyBytes = 2 << 20

func (s *Server) githubWebhook(c echo.Context) error {
	secret := strings.TrimSpace(s.githubWebhookSecret)
	if secret == "" {
		return c.JSON(http.StatusNotFound, errorResponse("webhook_not_configured", "GitHub webhook is not configured"))
	}
	if s.refresher == nil {
		return c.JSON(http.StatusServiceUnavailable, errorResponse("orchestrator_unavailable", "Orchestrator is unavailable"))
	}

	req := c.Request()
	req.Body = http.MaxBytesReader(c.Response(), req.Body, githubWebhookMaxBodyBytes)
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse("invalid_payload", "Webhook payload is invalid"))
	}
	if !validGitHubWebhookSignature(secret, body, req.Header.Get("X-Hub-Signature-256")) {
		return c.JSON(http.StatusUnauthorized, errorResponse("invalid_signature", "Webhook signature is invalid"))
	}

	target, err := githubWebhookTarget(req.Header.Get("X-GitHub-Event"), req.Header.Get("X-GitHub-Delivery"), body)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse("invalid_payload", "Webhook payload is invalid"))
	}
	response, err := s.requestWebhookRefresh(req.Context(), target)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, errorResponse("orchestrator_unavailable", "Orchestrator is unavailable"))
	}
	if response.RequestedAt.IsZero() {
		response.RequestedAt = apiNow()
	}
	response.Operations = prependOperation(response.Operations, "webhook:"+target.Event)
	return c.JSON(http.StatusAccepted, response)
}

func validGitHubWebhookSignature(secret string, body []byte, signature string) bool {
	signature = strings.TrimSpace(signature)
	value, ok := strings.CutPrefix(signature, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(value)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func githubWebhookTarget(event string, deliveryID string, body []byte) (RefreshTarget, error) {
	var payload struct {
		Repository *struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Issue *struct {
			Number int `json:"number"`
		} `json:"issue"`
		PullRequest *struct {
			Number int `json:"number"`
			Head   struct {
				SHA string `json:"sha"`
			} `json:"head"`
		} `json:"pull_request"`
		CheckRun *struct {
			HeadSHA      string `json:"head_sha"`
			PullRequests []struct {
				Number int `json:"number"`
			} `json:"pull_requests"`
		} `json:"check_run"`
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return RefreshTarget{}, err
	}
	target := RefreshTarget{
		Event:      strings.TrimSpace(event),
		DeliveryID: strings.TrimSpace(deliveryID),
	}
	if payload.Repository != nil {
		target.Repository = strings.TrimSpace(payload.Repository.FullName)
	}
	if payload.Issue != nil {
		target.IssueNumber = payload.Issue.Number
	}
	if payload.PullRequest != nil {
		target.PullRequestNumber = payload.PullRequest.Number
		target.SHA = strings.TrimSpace(payload.PullRequest.Head.SHA)
	}
	if payload.CheckRun != nil {
		target.SHA = strings.TrimSpace(payload.CheckRun.HeadSHA)
		if len(payload.CheckRun.PullRequests) > 0 {
			target.PullRequestNumber = payload.CheckRun.PullRequests[0].Number
		}
	}
	if target.SHA == "" {
		target.SHA = strings.TrimSpace(payload.SHA)
	}
	if target.Repository == "" {
		return RefreshTarget{}, echo.ErrBadRequest
	}
	if target.Event == "" {
		target.Event = "unknown"
	}
	return target, nil
}

func (s *Server) requestWebhookRefresh(ctx context.Context, target RefreshTarget) (RefreshResponse, error) {
	if refresher, ok := s.refresher.(TargetedRefresher); ok {
		return refresher.RequestTargetedRefresh(ctx, target)
	}
	return s.refresher.RequestRefresh(ctx)
}

func prependOperation(operations []string, operation string) []string {
	operation = strings.TrimSpace(operation)
	if operation == "" {
		return operations
	}
	for _, existing := range operations {
		if existing == operation {
			return operations
		}
	}
	return append([]string{operation}, operations...)
}
