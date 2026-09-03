package hubserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

const githubWebhookMaxBodyBytes = 2 << 20

type webhookReceipt struct {
	DeliveryID       string
	EventType        string
	Action           string
	HeadersJSON      string
	Payload          []byte
	PayloadSHA256    string
	ReceivedAt       time.Time
	PayloadExpiresAt time.Time
}

type webhookReceiptResult struct {
	InboxID   int64
	Duplicate bool
}

type webhookResponse struct {
	DeliveryID string `json:"delivery_id"`
	Duplicate  bool   `json:"duplicate"`
}

type webhookErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (s *Service) githubWebhook(c echo.Context) error {
	if len(s.config.GitHubWebhookSecret) == 0 {
		return c.JSON(http.StatusServiceUnavailable, webhookErrorResponse{
			Code:    "webhook_unavailable",
			Message: "GitHub webhook verification is not configured",
		})
	}

	request := c.Request()
	request.Body = http.MaxBytesReader(c.Response(), request.Body, githubWebhookMaxBodyBytes)
	payload, err := io.ReadAll(request.Body)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			return c.JSON(http.StatusRequestEntityTooLarge, webhookErrorResponse{
				Code:    "payload_too_large",
				Message: "GitHub webhook payload is too large",
			})
		}
		return c.JSON(http.StatusBadRequest, webhookErrorResponse{
			Code:    "invalid_payload",
			Message: "GitHub webhook payload is invalid",
		})
	}

	deliveryID := strings.TrimSpace(request.Header.Get("X-GitHub-Delivery"))
	eventType := strings.TrimSpace(request.Header.Get("X-GitHub-Event"))
	if deliveryID == "" || eventType == "" {
		return c.JSON(http.StatusBadRequest, webhookErrorResponse{
			Code:    "invalid_headers",
			Message: "GitHub webhook delivery and event headers are required",
		})
	}
	if !validGitHubWebhookSignature(s.config.GitHubWebhookSecret, payload, request.Header.Get("X-Hub-Signature-256")) {
		return c.JSON(http.StatusUnauthorized, webhookErrorResponse{
			Code:    "invalid_signature",
			Message: "GitHub webhook signature is invalid",
		})
	}
	if !json.Valid(payload) {
		return c.JSON(http.StatusBadRequest, webhookErrorResponse{
			Code:    "invalid_payload",
			Message: "GitHub webhook payload is invalid",
		})
	}

	var metadata struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(payload, &metadata); err != nil {
		return c.JSON(http.StatusBadRequest, webhookErrorResponse{
			Code:    "invalid_payload",
			Message: "GitHub webhook payload is invalid",
		})
	}
	headersJSON, err := json.Marshal(map[string]string{
		"content_type":        request.Header.Get("Content-Type"),
		"hook_id":             request.Header.Get("X-GitHub-Hook-ID"),
		"installation_target": request.Header.Get("X-GitHub-Hook-Installation-Target-ID"),
		"user_agent":          request.Header.Get("User-Agent"),
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, webhookErrorResponse{
			Code:    "receipt_failed",
			Message: "GitHub webhook delivery could not be recorded",
		})
	}
	payloadHash := sha256.Sum256(payload)
	now := s.config.now().UTC()
	receipt := webhookReceipt{
		DeliveryID:       deliveryID,
		EventType:        eventType,
		Action:           strings.TrimSpace(metadata.Action),
		HeadersJSON:      string(headersJSON),
		Payload:          payload,
		PayloadSHA256:    hex.EncodeToString(payloadHash[:]),
		ReceivedAt:       now,
		PayloadExpiresAt: now.Add(s.config.WebhookPayloadRetention),
	}
	result, err := s.database.recordWebhook(request.Context(), receipt)
	if errors.Is(err, ErrWebhookDeliveryConflict) {
		return c.JSON(http.StatusConflict, webhookErrorResponse{
			Code:    "delivery_conflict",
			Message: "GitHub webhook delivery ID conflicts with an earlier delivery",
		})
	}
	if err != nil {
		s.config.Logger.Error("record GitHub webhook", "delivery_id", deliveryID, "error", err)
		return c.JSON(http.StatusServiceUnavailable, webhookErrorResponse{
			Code:    "receipt_failed",
			Message: "GitHub webhook delivery could not be recorded",
		})
	}
	if err := s.database.processWebhook(request.Context(), result.InboxID, now); err != nil {
		s.config.Logger.Error("process GitHub webhook", "delivery_id", deliveryID, "error", err)
	}

	return c.JSON(http.StatusAccepted, webhookResponse{
		DeliveryID: deliveryID,
		Duplicate:  result.Duplicate,
	})
}

func validGitHubWebhookSignature(secret []byte, body []byte, signature string) bool {
	value, ok := strings.CutPrefix(strings.TrimSpace(signature), "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(value)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	if _, err := mac.Write(body); err != nil {
		return false
	}
	return hmac.Equal(got, mac.Sum(nil))
}
