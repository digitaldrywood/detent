package auth

import "context"

// InvitationAdministration is optional for providers which offer administration
// by identifier. Login invitation tokens stay in the existing browser exchange.
type InvitationAdministration interface {
	InvitationByID(context.Context, string) (Invitation, error)
	AcceptInvitationByID(context.Context, string, string) error
}

func LookupInvitationID(ctx context.Context, provider HostedProvider, id string) (Invitation, error) {
	commands, ok := provider.(InvitationAdministration)
	if !ok {
		return Invitation{}, ErrHostedIdentity
	}
	return commands.InvitationByID(ctx, id)
}
func AcceptInvitationID(ctx context.Context, provider HostedProvider, id, user string) error {
	commands, ok := provider.(InvitationAdministration)
	if !ok {
		return ErrHostedIdentity
	}
	return commands.AcceptInvitationByID(ctx, id, user)
}
