package hubserver

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/auth"
)

type invitationMCPProvider struct {
	*hostedSecurityProvider
	sends, resends int
	fail           bool
}

func (p *invitationMCPProvider) Invite(ctx context.Context, organization, email, role, user string) (auth.Invitation, error) {
	p.sends++
	if p.fail {
		return auth.Invitation{}, errors.New("private-delivery-sentinel")
	}
	return p.hostedSecurityProvider.Invite(ctx, organization, email, role, user)
}

func (p *invitationMCPProvider) ResendInvitation(ctx context.Context, id string) error {
	p.resends++
	if p.fail {
		return errors.New("private-delivery-sentinel")
	}
	return p.hostedSecurityProvider.ResendInvitation(ctx, id)
}
