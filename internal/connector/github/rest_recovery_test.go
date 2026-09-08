package github

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type recoveryHTTPClient func(*http.Request) (*http.Response, error)

func (f recoveryHTTPClient) Do(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestRESTRecoveryPreservesActualExhaustion(t *testing.T) {
	t.Parallel()
	for _, remaining := range []int{5000, 4000} {
		t.Run(strconv.Itoa(remaining), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				reset := time.Now().Add(13 * time.Minute).Truncate(time.Second)
				reads, probes := 0, 0
				healthy := false
				conn, err := NewConnector(Config{
					Endpoint:    "https://example.test/graphql",
					TokenSource: StaticTokenSource("test-token"),
					HTTPClient: recoveryHTTPClient(func(r *http.Request) (*http.Response, error) {
						headers := http.Header{}
						headers.Set("X-RateLimit-Limit", "5000")
						headers.Set("X-RateLimit-Resource", "core")
						status := http.StatusOK
						if r.URL.Path == "/rate_limit" {
							probes++
							headers.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
							headers.Set("X-RateLimit-Used", strconv.Itoa(5000-remaining))
							headers.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
						} else {
							if r.Method != http.MethodGet || r.URL.Path != "/repos/example/repo/issues" {
								t.Fatalf("unexpected recovery request: %s %s", r.Method, r.URL.Path)
							}
							reads++
							if healthy {
								headers.Set("X-RateLimit-Remaining", "5000")
								headers.Set("X-RateLimit-Used", "0")
								reset = time.Now().Add(time.Hour)
							} else {
								status = http.StatusForbidden
								headers.Set("X-RateLimit-Remaining", "0")
								headers.Set("X-RateLimit-Used", "5000")
							}
							headers.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
						}
						return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
					}),
				})
				if err != nil {
					t.Fatal(err)
				}
				conn.client.restBackoffs = newRESTBackoffRegistry()
				if err := conn.client.REST(t.Context(), http.MethodGet, "/repos/example/repo/issues", nil, nil); !errors.Is(err, ErrRateLimited) {
					t.Fatalf("initial request = %v", err)
				}
				for range 5 {
					time.Sleep(35 * time.Second)
					if _, err := conn.ProbeRESTRateLimit(t.Context(), 1000); !errors.Is(err, ErrRateLimited) {
						t.Errorf("contradictory probe = %v, want continued throttling", err)
					}
					usage := conn.RESTRateLimitStatus()
					if usage.RateLimit.Remaining != 0 || !usage.BackoffUntil.Equal(reset) {
						t.Errorf("exhaustion overwritten: %+v", usage)
					}
				}
				if reads != 1 || probes > 5 {
					t.Fatalf("requests before reset: reads=%d probes=%d", reads, probes)
				}
				time.Sleep(time.Until(reset) + time.Second)
				if _, err := conn.ProbeRESTRateLimit(t.Context(), 1000); !errors.Is(err, ErrRateLimited) {
					t.Fatalf("repeated real throttling = %v", err)
				}
				healthy = true
				time.Sleep(time.Minute + time.Second)
				quota, err := conn.ProbeRESTRateLimit(t.Context(), 1000)
				if err != nil || quota.Remaining != 5000 {
					t.Fatalf("representative recovery = %+v, %v", quota, err)
				}
				if conn.RESTRateLimitStatus().RateLimited {
					t.Fatal("healthy representative read did not recover")
				}
			})
		})
	}
}

func TestRESTRecoveryEvidence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name              string
		omit              string
		value             string
		credentialChanged bool
		concurrentFailure bool
		wantErr           error
	}{
		{name: "new window with unused bucket"},
		{name: "changed credential before reset", credentialChanged: true},
		{name: "missing limit", omit: "X-RateLimit-Limit", wantErr: ErrInvalidResponse},
		{name: "missing remaining", omit: "X-RateLimit-Remaining", wantErr: ErrInvalidResponse},
		{name: "missing reset", omit: "X-RateLimit-Reset", wantErr: ErrInvalidResponse},
		{name: "missing resource", omit: "X-RateLimit-Resource", wantErr: ErrInvalidResponse},
		{name: "wrong resource", omit: "X-RateLimit-Resource", value: "search", wantErr: ErrInvalidResponse},
		{name: "expired window", omit: "X-RateLimit-Reset", value: "1", wantErr: ErrInvalidResponse},
		{name: "invalid capacity", omit: "X-RateLimit-Remaining", value: "5001", wantErr: ErrInvalidResponse},
		{name: "newer shared failure", concurrentFailure: true, wantErr: ErrRateLimited},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				registry := newRESTBackoffRegistry()
				seed := true
				var conn *Connector
				conn, err := NewConnector(Config{
					Endpoint:    "https://evidence.test/graphql",
					TokenSource: StaticTokenSource("old-token"),
					HTTPClient: recoveryHTTPClient(func(r *http.Request) (*http.Response, error) {
						headers := http.Header{}
						headers.Set("X-RateLimit-Limit", "5000")
						headers.Set("X-RateLimit-Remaining", "5000")
						headers.Set("X-RateLimit-Used", "0")
						headers.Set("X-RateLimit-Resource", "core")
						headers.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
						status := http.StatusOK
						if seed {
							status = http.StatusForbidden
							headers.Set("X-RateLimit-Remaining", "0")
						} else if r.URL.Path != "/rate_limit" {
							if tt.omit != "" {
								headers.Del(tt.omit)
								if tt.value != "" {
									headers.Set(tt.omit, tt.value)
								}
							}
							if tt.concurrentFailure {
								registry.set(conn.client.restSharedBackoffKey("old-token"), time.Now().Add(time.Minute), http.MethodGet, "/user", "core")
							}
						}
						return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
					}),
				})
				if err != nil {
					t.Fatal(err)
				}
				conn.client.restBackoffs = registry
				if err := conn.client.REST(t.Context(), http.MethodGet, "/user", nil, nil); !errors.Is(err, ErrRateLimited) {
					t.Fatal(err)
				}
				seed = false
				if tt.credentialChanged {
					conn.client.tokenSource = StaticTokenSource("new-token")
				} else {
					time.Sleep(time.Hour + time.Second)
				}
				_, err = conn.ProbeRESTRateLimit(t.Context(), 1000)
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("recovery error = %v, want %v", err, tt.wantErr)
				}
				if tt.wantErr != nil && registry.failure(conn.client.restSharedBackoffKey("old-token")) == nil {
					t.Fatal("indeterminate recovery discarded exhaustion evidence")
				}
				if tt.credentialChanged && !registry.until(conn.client.restSharedBackoffKey("old-token"), time.Now()).After(time.Now()) {
					t.Fatal("changed credential cleared old credential backoff")
				}
			})
		})
	}
}

func TestRESTRecoveryFallbackMatchesCredential(t *testing.T) {
	t.Parallel()
	for _, installation := range []bool{false, true} {
		t.Run(strconv.FormatBool(installation), func(t *testing.T) {
			t.Parallel()
			source := StaticTokenSource("test-token")
			path := "/user"
			if installation {
				source = &InstallationTokenSource{installationID: "4242", cachedToken: "test-token", expiresAt: time.Now().Add(time.Hour), now: time.Now, details: InstallationTokenDetails{Token: "test-token"}}
				path = "/installation/repositories?per_page=1"
			}
			reads := 0
			conn, err := NewConnector(Config{
				Endpoint:    "https://fallback.test/graphql",
				TokenSource: source,
				HTTPClient: recoveryHTTPClient(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path != "/rate_limit" {
						reads++
						if r.URL.RequestURI() != path {
							t.Errorf("recovery path = %s, want %s", r.URL.RequestURI(), path)
						}
					}
					headers := http.Header{}
					headers.Set("X-RateLimit-Limit", "5000")
					headers.Set("X-RateLimit-Remaining", "5000")
					headers.Set("X-RateLimit-Resource", "core")
					headers.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
					return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			conn.client.restBackoffs = newRESTBackoffRegistry()
			if _, err := conn.ProbeRESTRateLimit(t.Context(), 1000); err != nil {
				t.Fatal(err)
			}
			if reads != 1 {
				t.Fatalf("representative reads = %d, want 1", reads)
			}
		})
	}
}
