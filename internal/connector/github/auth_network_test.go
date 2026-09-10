package github

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

type authPreflightTransport func(*http.Request) (*http.Response, error)

func (f authPreflightTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAuthenticateNetworkClassification(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		transportErr error
		status       int
		want         error
		retryable    bool
	}{
		{name: "DNS unavailable", transportErr: &net.DNSError{Err: "no such host", Name: "api.github.com"}, want: ErrTransient, retryable: true},
		{name: "dial unavailable", transportErr: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("network unreachable")}, want: ErrTransient, retryable: true},
		{name: "unauthorized", status: http.StatusUnauthorized, want: ErrAuthenticationFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewConnector(Config{APIKey: "test-token", ProjectSlug: "PVT_1", HTTPClient: &http.Client{Transport: authPreflightTransport(func(*http.Request) (*http.Response, error) {
				if tt.transportErr != nil {
					return nil, tt.transportErr
				}
				return &http.Response{StatusCode: tt.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":"Bad credentials"}`))}, nil
			})}, GHToken: func(context.Context, string) (string, error) { return "", errors.New("no fallback token") }})
			if err != nil {
				t.Fatal(err)
			}
			err = c.Authenticate(t.Context())
			if !errors.Is(err, tt.want) || connector.IsRetryable(err) != tt.retryable {
				t.Fatalf("Authenticate = %v; retryable = %v", err, connector.IsRetryable(err))
			}
		})
	}
}
