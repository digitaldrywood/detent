package auth

import (
	"context"
	"net/url"
	"strings"
	"time"
)

const InvitationUnavailable = "This invitation is unavailable."

// InvitationProblem returns public copy without disclosing the invited address.
// An accepted invitation may finish its protected callback for the same subject;
// it cannot start another login (where subject is empty).
func InvitationProblem(invitation Invitation, subject, email string, now time.Time) string {
	if email != "" && !strings.EqualFold(invitation.Email, email) {
		if _, domain, ok := strings.Cut(invitation.Email, "@"); ok && domain != "" {
			return "This invitation was sent to a different account at " + domain + "."
		}
		return "This invitation was sent to a different account."
	}
	if invitation.State == "accepted" && (subject == "" || invitation.AcceptedUserID != subject) {
		return "This invitation has already been used."
	}
	if !invitation.ExpiresAt.After(now) || invitation.State == "expired" {
		return "This invitation has expired."
	}
	if invitation.State != "pending" && invitation.State != "accepted" {
		return InvitationUnavailable
	}
	return ""
}

// InvitationAuthorizationURL opens AuthKit on sign-up only for a new account.
// The callback still verifies the recipient and the locally issued invitation.
func InvitationAuthorizationURL(ctx context.Context, provider HostedProvider, invitation Invitation, token, state, verifier string) (string, error) {
	exists, err := provider.HasUser(ctx, invitation.Email)
	if err != nil {
		return "", err
	}
	target, err := url.Parse(provider.AuthorizationURL(state, state, verifier))
	if err != nil {
		return "", err
	}
	query := target.Query()
	query.Set("invitation_token", token)
	query.Del("screen_hint")
	if !exists {
		query.Set("screen_hint", "sign-up")
	}
	target.RawQuery = query.Encode()
	return target.String(), nil
}
