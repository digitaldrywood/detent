package entryauthority

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
)

type ArchiveRequest struct {
	Reference attachment.ArchiveReference
	Presign   bool
	Deadline  time.Time
}

type ArchiveResponse struct {
	Data   []byte
	Access *attachment.ArchiveRead
}

type ArchiveReader func(context.Context, attachment.ArchiveReference, bool) (ArchiveResponse, error)

func (s *authorityServer) ReadArchive(request ArchiveRequest, result *ArchiveResponse) error {
	if s.archive == nil || request.Deadline.IsZero() || request.Reference.Validate(s.binding.Organization) != nil || request.Reference.UncompressedSize > attachment.MaxSectionBytes {
		return cloudassert.ErrInvalid
	}
	response, err := s.archive(request)
	if err != nil || int64(len(response.Data)) != request.Reference.CompressedSize {
		return cloudassert.ErrInvalid
	}
	*result = response
	return nil
}

func (c *Client) ReadArchive(ctx context.Context, ref attachment.ArchiveReference, presign bool) (ArchiveResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, c.budget)
	defer cancel()
	deadline, _ := ctx.Deadline()
	return callAuthority[ArchiveResponse](ctx, c, "ReadArchive", ArchiveRequest{Reference: ref, Presign: presign, Deadline: deadline})
}
