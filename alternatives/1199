package linear

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

type trackerAvailabilityHTTPClient struct {
	do func(*http.Request) (*http.Response, error)
}

func (c trackerAvailabilityHTTPClient) Do(request *http.Request) (*http.Response, error) {
	return c.do(request)
}

func TestClientTrackerAvailabilityClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		query            string
		status           int
		transportErr     error
		wantAvailability bool
		wantClass        string
	}{
		{name: "server error", query: "query DetentIssues { issues { nodes { id } } }", status: http.StatusBadGateway, wantAvailability: true, wantClass: connector.TrackerAvailabilityClassServer},
		{name: "timeout", query: "query DetentIssues { issues { nodes { id } } }", transportErr: context.DeadlineExceeded, wantAvailability: true, wantClass: connector.TrackerAvailabilityClassTimeout},
		{name: "dns", query: "query DetentIssues { issues { nodes { id } } }", transportErr: &net.DNSError{Err: "no such host", Name: "api.linear.test"}, wantAvailability: true, wantClass: connector.TrackerAvailabilityClassTransport},
		{name: "transport", query: "query DetentIssues { issues { nodes { id } } }", transportErr: errors.New("tls handshake failed"), wantAvailability: true, wantClass: connector.TrackerAvailabilityClassTransport},
		{name: "rate limit", query: "query DetentIssues { issues { nodes { id } } }", status: http.StatusTooManyRequests},
		{name: "forbidden", query: "query DetentIssues { issues { nodes { id } } }", status: http.StatusForbidden},
		{name: "not found", query: "query DetentIssues { issues { nodes { id } } }", status: http.StatusNotFound},
		{name: "mutation server error", query: "mutation DetentMove { issueUpdate(id: \"1\") { success } }", status: http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client, err := NewClient(ClientConfig{
				Endpoint: "https://api.linear.test/graphql",
				APIKey:   "test-key",
				HTTPClient: trackerAvailabilityHTTPClient{do: func(request *http.Request) (*http.Response, error) {
					if tt.transportErr != nil {
						return nil, tt.transportErr
					}
					return &http.Response{
						StatusCode: tt.status,
						Body:       io.NopCloser(strings.NewReader(`{"message":"unavailable"}`)),
						Header:     http.Header{},
						Request:    request,
					}, nil
				}},
			})
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}

			err = client.GraphQL(context.Background(), tt.query, nil, nil)
			availabilityErr, gotAvailability := connector.AsTrackerAvailability(err)
			if gotAvailability != tt.wantAvailability {
				t.Fatalf("tracker availability = %v, want %v; error = %v", gotAvailability, tt.wantAvailability, err)
			}
			if gotAvailability && availabilityErr.Class != tt.wantClass {
				t.Fatalf("class = %q, want %q", availabilityErr.Class, tt.wantClass)
			}
		})
	}
}
