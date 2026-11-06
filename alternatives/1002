package entryauthority

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
)

type DetailBatchRequest struct {
	Organization string
	References   []attachment.ArchiveReference
	Deadline     time.Time
}

func (s *authorityServer) ReadDetails(request DetailBatchRequest, result *[]attachment.DetailReadResult) error {
	if s.readDetails == nil || request.Organization != s.binding.Organization || request.Deadline.IsZero() || attachment.ValidateDetailBatch(s.binding.Organization, request.References) != nil {
		return attachment.ErrInvalid
	}
	rows, err := s.readDetails(request)
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
	ctx, cancel := context.WithTimeout(ctx, c.budget)
	defer cancel()
	deadline, _ := ctx.Deadline()
	rows, err := callAuthority[[]attachment.DetailReadResult](ctx, c, "ReadDetails", DetailBatchRequest{Organization: organization, References: refs, Deadline: deadline})
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
