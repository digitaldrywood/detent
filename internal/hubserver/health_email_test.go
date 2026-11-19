package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/auth"
)

type healthEmailSenderFunc func(context.Context, auth.EmailMessage) error

func (f healthEmailSenderFunc) SendEmail(ctx context.Context, message auth.EmailMessage) error {
	return f(ctx, message)
}
