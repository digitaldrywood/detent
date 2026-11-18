package hubserver

import (
	"context"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/auth"
)

type healthEmailSenderFunc func(context.Context, auth.EmailMessage) error

func (f healthEmailSenderFunc) SendEmail(ctx context.Context, message auth.EmailMessage) error {
	return f(ctx, message)
}

func TestHealthEmailTextRedactsCredentials(t *testing.T) {
	t.Parallel()
	for _, credential := range []string{
		"password=hunter2", "api_key=private-key", "token=private-token", "secret='private value'", "Authorization: Bearer abc.def", "ghp_abcdefghijklmnopqrstuvwxyz123456789", "github_pat_private", "sk-proj-private", "AKIA1234567890123456", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.private", "https://user:password@example.test/path?token=private", "0123456789abcdefghijklmnopqrstuvwxyz",
	} {
		if got := healthEmailText(credential); strings.Contains(got, credential) || !strings.Contains(got, "[redacted]") {
			t.Fatalf("credential not redacted: %q -> %q", credential, got)
		}
	}
}
