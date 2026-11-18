package auth_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
)

type membershipTransport func(*http.Request) (*http.Response, error)

func (f membershipTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWorkOSMembershipCache(t *testing.T) {
	for _, test := range []struct {
		name           string
		warm           bool
		outage         bool
		advance        time.Duration
		revoked        bool
		wantCalls      int
		wantMembers    int
		wantReason     string
		userID         string
		organizationID string
	}{
		{name: "fresh lookup", wantCalls: 1, wantMembers: 1},
		{name: "cached hit", warm: true, wantCalls: 1, wantMembers: 1},
		{name: "cached during transport outage", warm: true, outage: true, advance: 59 * time.Second, wantCalls: 1, wantMembers: 1},
		{name: "expiry refreshes", warm: true, advance: time.Minute, wantCalls: 2, wantMembers: 1},
		{name: "expiry observes revocation", warm: true, advance: time.Minute, revoked: true, wantCalls: 2},
		{name: "expired cache cannot authorize outage", warm: true, outage: true, advance: time.Minute, wantCalls: 2, wantReason: auth.HostedReasonProviderUnavailable},
		{name: "uncached outage", outage: true, wantCalls: 1, wantReason: auth.HostedReasonProviderUnavailable},
		{name: "different principal cannot use cached membership", warm: true, outage: true, userID: "user_other", wantCalls: 2, wantReason: auth.HostedReasonProviderUnavailable},
		{name: "different organization cannot use cached membership", warm: true, outage: true, organizationID: "org_other", wantCalls: 2, wantReason: auth.HostedReasonProviderUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls, outage, revoked := 0, false, false
				provider, err := auth.NewHostedProvider("workos", auth.WorkOSConfig{
					ClientID: "client_example", APIKey: "fixture-secret", RedirectURL: "https://app.example.com/callback",
					HTTPClient: &http.Client{Transport: membershipTransport(func(r *http.Request) (*http.Response, error) {
						calls++
						if outage {
							return nil, errors.New("dial: connection reset")
						}
						body := `{"data":[{"id":"membership_example","user_id":"user_example","organization_id":"org_example","status":"active","role":{"slug":"owner"}}]}`
						if revoked {
							body = `{"data":[]}`
						}
						return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
					})},
				})
				if err != nil {
					t.Fatal(err)
				}
				if test.warm {
					members, err := provider.Memberships(t.Context(), "user_example", "org_example")
					if err != nil || len(members) != 1 {
						t.Fatalf("warm lookup: members=%v err=%v", members, err)
					}
					members[0].Role.Slug = "viewer"
				}
				outage, revoked = test.outage, test.revoked
				time.Sleep(test.advance)
				userID, organizationID := "user_example", "org_example"
				if test.userID != "" {
					userID = test.userID
				}
				if test.organizationID != "" {
					organizationID = test.organizationID
				}
				members, err := provider.Memberships(t.Context(), userID, organizationID)
				if calls != test.wantCalls || len(members) != test.wantMembers || auth.HostedIdentityReason(err) != test.wantReason {
					t.Fatalf("calls=%d members=%v err=%v; want calls=%d members=%d reason=%q", calls, members, err, test.wantCalls, test.wantMembers, test.wantReason)
				}
				if len(members) != 0 && members[0].Role.Slug != "owner" {
					t.Fatalf("caller changed cached role: %v", members)
				}
			})
		})
	}
}

func TestWorkOSTransportFailureLogging(t *testing.T) {
	for _, path := range []string{"membership", "invitation"} {
		t.Run(path, func(t *testing.T) {
			var output bytes.Buffer
			provider, err := auth.NewHostedProvider("workos", auth.WorkOSConfig{
				ClientID: "client_example", APIKey: "fixture-secret", RedirectURL: "https://app.example.com/callback",
				Logger: slog.New(slog.NewJSONHandler(&output, nil)),
				HTTPClient: &http.Client{Transport: membershipTransport(func(*http.Request) (*http.Response, error) {
					return nil, errors.New("dial: TLS handshake timeout")
				})},
			})
			if err != nil {
				t.Fatal(err)
			}
			wantPath := "/user_management/organization_memberships"
			if path == "membership" {
				_, err = provider.Memberships(t.Context(), "user_example", "org_example")
			} else {
				wantPath = "/user_management/invitations/by_token/redacted"
				_, err = provider.Invitation(t.Context(), "invitation_secret")
			}
			if auth.HostedIdentityReason(err) != auth.HostedReasonProviderUnavailable {
				t.Fatalf("reason: %v", err)
			}
			var record map[string]any
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatalf("log=%s err=%v", output.String(), err)
			}
			if record["level"] != "WARN" || record["method"] != "GET" || record["path"] != wantPath || record["elapsed"] == nil || !strings.Contains(record["error"].(string), "dial: TLS handshake timeout") {
				t.Fatalf("log=%s", output.String())
			}
			for _, secret := range []string{"fixture-secret", "invitation_secret", "user_id=", "organization_id="} {
				if strings.Contains(output.String(), secret) {
					t.Fatalf("log exposed %q: %s", secret, output.String())
				}
			}
		})
	}
}

func TestWorkOSMembershipMutationInvalidatesCache(t *testing.T) {
	for _, operation := range []string{"create", "role", "revoke", "revoke during lookup"} {
		t.Run(operation, func(t *testing.T) {
			var outage atomic.Bool
			started, release := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			provider, err := auth.NewHostedProvider("workos", auth.WorkOSConfig{
				ClientID: "client_example", APIKey: "fixture-secret", RedirectURL: "https://app.example.com/callback",
				HTTPClient: &http.Client{Transport: membershipTransport(func(r *http.Request) (*http.Response, error) {
					body := `{"id":"membership_example","user_id":"user_example","organization_id":"org_example","status":"active","role":{"slug":"owner"}}`
					if r.Method == http.MethodGet {
						if outage.Load() {
							return nil, errors.New("dial: connection reset")
						}
						if operation == "revoke during lookup" {
							close(started)
							<-release
						}
						body = `{"data":[` + body + `]}`
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				})},
			})
			if err != nil {
				t.Fatal(err)
			}
			loaded := make(chan error, 1)
			go func() { _, err := provider.Memberships(t.Context(), "user_example", "org_example"); loaded <- err }()
			if operation == "revoke during lookup" {
				<-started
			} else if err := <-loaded; err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "create":
				_, err = provider.CreateMembership(t.Context(), "user_example", "org_example", "owner")
			case "role":
				err = provider.SetMembershipRole(t.Context(), "membership_example", "owner")
			default:
				err = provider.RevokeMembership(t.Context(), "membership_example")
			}
			if err != nil {
				t.Fatal(err)
			}
			if operation == "revoke during lookup" {
				close(release)
				if err := <-loaded; err != nil {
					t.Fatal(err)
				}
			}
			outage.Store(true)
			if _, err := provider.Memberships(t.Context(), "user_example", "org_example"); auth.HostedIdentityReason(err) != auth.HostedReasonProviderUnavailable {
				t.Fatalf("mutation retained cached membership: %v", err)
			}
		})
	}
}
