package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
)

const (
	HostedReasonUnknown              = "unknown"
	HostedReasonCodeMissing          = "code_missing"
	HostedReasonExchangeFailed       = "exchange_failed"
	HostedReasonTokenInvalid         = "token_invalid"
	HostedReasonIssuerMismatch       = "issuer_mismatch"
	HostedReasonClientMismatch       = "client_mismatch"
	HostedReasonAudienceMismatch     = "audience_mismatch"
	HostedReasonSubjectMismatch      = "subject_mismatch"
	HostedReasonOrganizationMismatch = "organization_mismatch"
	HostedReasonPKCEMissing          = "pkce_missing"
	HostedReasonEmailUnverified      = "email_unverified"
	HostedReasonEmailInvalid         = "email_invalid"
	HostedReasonSessionNotFound      = "session_not_found"
	HostedReasonSessionInvalid       = "session_invalid"
	HostedReasonSessionExpired       = "session_expired"
	HostedReasonSessionChanged       = "session_changed"
	HostedReasonSupportActorInvalid  = "support_actor_invalid"
	HostedReasonProviderUnavailable  = "provider_unavailable"
	HostedReasonProviderRejected     = "provider_rejected"
	HostedReasonProviderInvalid      = "provider_response_invalid"
)

type HostedIdentityError struct {
	Reason      string
	Status      int
	TokenIssuer string
}

func (e *HostedIdentityError) Error() string {
	message := ErrHostedIdentity.Error() + ": " + e.Reason
	if e.Status != 0 {
		message += " (status " + strconv.Itoa(e.Status) + ")"
	}
	return message
}

func (e *HostedIdentityError) Unwrap() error {
	return ErrHostedIdentity
}

func hostedDenial(reason string) error {
	return &HostedIdentityError{Reason: reason}
}

func hostedDenialWrap(reason string, cause error) error {
	var typed *HostedIdentityError
	if errors.As(cause, &typed) {
		return &HostedIdentityError{Reason: reason, Status: typed.Status, TokenIssuer: typed.TokenIssuer}
	}
	return hostedDenial(reason)
}

func HostedIdentityReason(err error) string {
	if err == nil {
		return ""
	}
	var typed *HostedIdentityError
	if errors.As(err, &typed) && typed.Reason != "" {
		return typed.Reason
	}
	return HostedReasonUnknown
}

func HostedIdentityDetails(err error) (status int, tokenIssuer string) {
	var typed *HostedIdentityError
	if errors.As(err, &typed) {
		return typed.Status, typed.TokenIssuer
	}
	return 0, ""
}

func EmailLogAttributes(email string) (domain string, hash string) {
	email = normalizeEmail(email)
	if email == "" {
		return "", ""
	}
	if at := strings.LastIndexByte(email, '@'); at >= 0 && at < len(email)-1 {
		domain = email[at+1:]
	}
	sum := sha256.Sum256([]byte(email))
	return domain, hex.EncodeToString(sum[:])[:12]
}
