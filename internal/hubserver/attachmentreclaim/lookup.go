package attachmentreclaim

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func Lookup(ctx context.Context, repo tenantstore.Repository, org tracker.OrganizationID, input attachment.ObjectLookup, now time.Time) (any, error) {
	if input.Receipt != nil {
		return true, repo.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error { return w.CompleteArchiveReclaim(ctx, *input.Receipt) })
	}
	var result any
	err := repo.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		switch {
		case input.Page:
			result, err = r.ArchiveReclaimPage(ctx, now)
		case input.Keys != nil:
			result, err = r.ReferencedAttachmentObjects(ctx, input.Keys)
		default:
			var exists bool
			exists, err = r.AttachmentObjectExists(ctx, input.ID, input.Key)
			result = map[string]bool{"exists": exists}
		}
		return err
	})
	return result, err
}
