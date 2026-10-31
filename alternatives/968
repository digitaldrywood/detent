package entryauthority

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
)

type IconRequest struct {
	Organization string
	ID           string
	Content      []byte
	Deadline     time.Time
}

func (s *authorityServer) PutIcon(request IconRequest, result *bool) error {
	if s.icons == nil || request.Organization != s.binding.Organization || request.Deadline.IsZero() || len(request.Content) > 256*1024 {
		return attachment.ErrInvalid
	}
	ctx, cancel := s.contentContext(request.Deadline)
	defer cancel()
	ctx, stop := context.WithTimeout(ctx, s.budget)
	defer stop()
	if err := s.icons.PutIcon(ctx, request.Organization, request.ID, request.Content); err != nil {
		return err
	}
	*result = true
	return nil
}

func (c *Client) PutIcon(ctx context.Context, organization, id string, content []byte) error {
	if organization != c.organization || len(content) > 256*1024 {
		return attachment.ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, c.budget)
	defer cancel()
	deadline, _ := ctx.Deadline()
	_, err := callAuthority[bool](ctx, c, "PutIcon", IconRequest{Organization: organization, ID: id, Content: content, Deadline: deadline})
	return err
}

func (s *authorityServer) DeleteIcon(request IconRequest, result *bool) error {
	if s.icons == nil || request.Organization != s.binding.Organization || request.Deadline.IsZero() || len(request.Content) != 0 {
		return attachment.ErrInvalid
	}
	ctx, cancel := s.contentContext(request.Deadline)
	defer cancel()
	ctx, stop := context.WithTimeout(ctx, s.budget)
	defer stop()
	if err := s.icons.DeleteIcon(ctx, request.Organization, request.ID); err != nil {
		return err
	}
	*result = true
	return nil
}

func (c *Client) DeleteIcon(ctx context.Context, organization, id string) error {
	if organization != c.organization {
		return attachment.ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, c.budget)
	defer cancel()
	deadline, _ := ctx.Deadline()
	_, err := callAuthority[bool](ctx, c, "DeleteIcon", IconRequest{Organization: organization, ID: id, Deadline: deadline})
	return err
}
