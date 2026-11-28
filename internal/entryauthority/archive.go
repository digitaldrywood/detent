package entryauthority

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
)

type ArchiveRequest struct {
	ReadControl
	Reference attachment.ArchiveReference
	Presign   bool
	Deadline  time.Time
}

func (r ArchiveRequest) readDeadline() time.Time { return r.Deadline }

type ArchiveResponse struct {
	Data   []byte
	Access *attachment.ArchiveRead
}

type ArchiveReader func(context.Context, attachment.ArchiveReference, bool) (ArchiveResponse, error)

func (s *authorityServer) ReadArchive(request ArchiveRequest, result *ArchiveResponse) error {
	ctx, cancel := request.start()
	defer cancel()
	if s.archive == nil || request.Deadline.IsZero() || request.Reference.Validate(s.binding.Organization) != nil || request.Reference.UncompressedSize > attachment.MaxSectionBytes {
		return cloudassert.ErrInvalid
	}
	response, err := s.archive(ctx, request)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || int64(len(response.Data)) != request.Reference.CompressedSize {
		return cloudassert.ErrInvalid
	}
	*result = response
	return nil
}

func (c *Client) ReadArchive(ctx context.Context, ref attachment.ArchiveReference, presign bool) (ArchiveResponse, error) {
	ctx, cancel := c.readContext(ctx)
	defer cancel()
	deadline, _ := ctx.Deadline()
	id := c.readID.Add(1)
	return callAuthorityRPC[ArchiveResponse](ctx, c.client, "ReadArchive", ArchiveRequest{ReadControl: ReadControl{ReadID: id}, Reference: ref, Presign: presign, Deadline: deadline}, id)
}
