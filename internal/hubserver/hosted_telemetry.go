package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

func (s *Service) reserveHostedInvitationSeatFor(ctx context.Context, credential hubapi.APICredential, email string) (bool, error) {
	return s.reserveAccessInvitationSeat(ctx, credential, email)
}
