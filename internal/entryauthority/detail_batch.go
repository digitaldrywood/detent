package entryauthority

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
)

type DetailBatchRequest struct {
	ReadControl
	Organization string
	References   []attachment.ArchiveReference
	Deadline     time.Time
}

func (r DetailBatchRequest) readDeadline() time.Time { return r.Deadline }

func (s *authorityServer) ReadDetails(request DetailBatchRequest, result *[]attachment.DetailReadResult) error {
	ctx, cancel := request.start()
	defer cancel()
	if s.readDetails == nil || request.Organization != s.binding.Organization || request.Deadline.IsZero() || attachment.ValidateDetailBatch(s.binding.Organization, request.References) != nil {
		return attachment.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	rows, err := s.readDetails(ctx, request)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return artifactDetailError()
	}
	if len(rows) != len(request.References) {
		return artifactDetailError()
	}
	for index, row := range rows {
		if row.Reference != request.References[index] {
			return artifactDetailError()
		}
		if row.ErrorCode != "" {
			rows[index].Data = nil
		} else if _, err := row.Bytes(); err != nil {
			rows[index].Data = nil
			rows[index].ErrorCode = "integrity"
		}
	}
	*result = rows
	return nil
}
func (c *Client) ReadDetails(ctx context.Context, organization string, refs []attachment.ArchiveReference) ([]attachment.DetailReadResult, error) {
	if organization != c.organization || attachment.ValidateDetailBatch(organization, refs) != nil {
		return nil, attachment.ErrInvalid
	}
	ctx, cancel := c.readContext(ctx)
	defer cancel()
	deadline, _ := ctx.Deadline()
	id := c.readID.Add(1)
	rows, err := callAuthorityRPC[[]attachment.DetailReadResult](ctx, c.client, "ReadDetails", DetailBatchRequest{ReadControl: ReadControl{ReadID: id}, Organization: organization, References: refs, Deadline: deadline}, id)
	if err != nil {
		return nil, contentClientError(err)
	}
	if len(rows) != len(refs) {
		return nil, artifact.ErrIntegrity
	}
	for index, row := range rows {
		if row.Reference != refs[index] {
			return nil, artifact.ErrIntegrity
		}
	}
	return rows, nil
}
