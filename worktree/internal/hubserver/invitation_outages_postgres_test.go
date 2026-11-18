package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
)

func (r entryAuthFaultReader) AccessInvitation(ctx context.Context, id string) (accessstore.Invitation, error) {
	if r.stage == "invitation" {
		return accessstore.Invitation{}, r.err
	}
	return r.Reader.AccessInvitation(ctx, id)
}
