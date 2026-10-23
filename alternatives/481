package auth

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
)

const RequestIDHeader = "X-Request-Id"

type HostedDenial struct {
	Flow          string
	Reason        string
	Status        int
	Err           error
	Email         string
	ProviderError string
}

func HostedRequestID(w http.ResponseWriter, r *http.Request) string {
	if id := w.Header().Get(RequestIDHeader); validRequestID(id) {
		return id
	}
	id := r.Header.Get(RequestIDHeader)
	if !validRequestID(id) {
		random := make([]byte, 8)
		if _, err := rand.Read(random); err != nil {
			return "unavailable"
		}
		id = hex.EncodeToString(random)
	}
	w.Header().Set(RequestIDHeader, id)
	return id
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, character := range id {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '-' && character != '_' && character != '.' {
			return false
		}
	}
	return true
}

func LogHostedDenial(logger *slog.Logger, w http.ResponseWriter, r *http.Request, denial HostedDenial) {
	if logger == nil {
		logger = slog.Default()
	}
	reason := denial.Reason
	if reason == "" {
		reason = HostedIdentityReason(denial.Err)
	}
	if reason == "" {
		reason = HostedReasonUnknown
	}
	attrs := []any{"reason", reason, "flow", denial.Flow, "request_id", HostedRequestID(w, r), "path", r.URL.Path, "http_status", denial.Status}
	if denial.Err != nil {
		if providerReason := HostedIdentityReason(denial.Err); providerReason != reason && providerReason != HostedReasonUnknown {
			attrs = append(attrs, "provider_reason", providerReason)
		}
		status, issuer := HostedIdentityDetails(denial.Err)
		if status != 0 {
			attrs = append(attrs, "provider_status", status)
		}
		if issuer != "" {
			attrs = append(attrs, "token_issuer", issuer)
		}
	}
	if code := ProviderErrorCode(denial.ProviderError); code != "" {
		attrs = append(attrs, "provider_error", code)
	}
	if domain, hash := EmailLogAttributes(denial.Email); hash != "" {
		attrs = append(attrs, "email_domain", domain, "email_hash", hash)
	}
	logger.WarnContext(r.Context(), "hosted sign-in denied", attrs...)
}

func ProviderErrorCode(value string) string {
	if value == "" {
		return ""
	}
	if len(value) > 64 {
		return "invalid"
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && character != '_' {
			return "invalid"
		}
	}
	return value
}
