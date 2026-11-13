package hubserver
import("context")
func (s *Service) reserveHostedInvitationSeatFor(ctx context.Context, credential apiCredential, email string) (bool, error) {
	return s.reserveAccessInvitationSeat(ctx, credential, email)
}

func (s *Service) releaseHostedInvitation(ctx context.Context, email string) error {

	return s.releaseAccessInvitation(ctx, email)
}
