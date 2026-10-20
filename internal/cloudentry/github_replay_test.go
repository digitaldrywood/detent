package cloudentry

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/digitaldrywood/detent/internal/connector/github"
)

type replayTestHandler struct {
	slog.Handler
	done chan slog.Record
}

func (h replayTestHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h replayTestHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message == "replay GitHub App deliveries" {
		h.done <- record
	}
	return nil
}

func TestSharedEntryGitHubStartupReplay(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	before := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	since := before.Add(-time.Hour)
	for _, test := range []struct {
		name          string
		noGap         bool
		noReceipt     bool
		receiptError  bool
		listError     bool
		postError     bool
		duplicate     bool
		recovered     bool
		wantIDs       []string
		wantPages     int
		wantRequested int64
		wantWarning   bool
	}{
		{name: "no gap", noGap: true},
		{name: "no accepted receipt", noReceipt: true},
		{name: "outage failures", wantIDs: []string{"101", "102"}, wantPages: 2, wantRequested: 2},
		{name: "duplicate delivery IDs", duplicate: true, wantIDs: []string{"102", "103"}, wantPages: 2, wantRequested: 2},
		{name: "successful redelivery", recovered: true, wantIDs: []string{"102"}, wantPages: 2, wantRequested: 1},
		{name: "GitHub API unavailable", listError: true, wantPages: 1, wantWarning: true},
		{name: "GitHub redelivery unavailable", postError: true, wantIDs: []string{"101"}, wantPages: 2, wantWarning: true},
		{name: "tenant receipt unavailable", receiptError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			done := make(chan slog.Record, 1)
			var receiptIDs []string
			service, err := Open(t.Context(), Config{
				Platform:  PlatformConfig{BootstrapAdminEmail: "bootstrap@example.test"},
				PublicURL: testPublicURL, ListenAddress: "127.0.0.1:0", Issuer: "entry",
				SigningKey: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)), Provider: newFakeProvider(),
				StateDir: t.TempDir(), clientFS: fstest.MapFS{},
				Logger:              slog.New(replayTestHandler{Handler: slog.DiscardHandler, done: done}),
				GitHubWebhookSecret: []byte("secret"), now: func() time.Time { return before },
				transport: func(organization Organization) (http.RoundTripper, error) {
					return handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path != "/internal/v1/github/receipt" {
							t.Errorf("unexpected tenant request %s", r.URL.Path)
							w.WriteHeader(http.StatusNotFound)
							return
						}
						receiptIDs = append(receiptIDs, organization.ID)
						if test.receiptError {
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
						var received *time.Time
						if !test.noReceipt {
							value := since
							if test.noGap {
								value = before
							}
							if organization.ID == "org_beta" {
								value = value.Add(-time.Hour)
							}
							received = &value
						}
						if err := json.NewEncoder(w).Encode(map[string]any{"received_at": received}); err != nil {
							t.Error(err)
						}
					})}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := service.Close(); err != nil {
					t.Error(err)
				}
			})
			for _, organization := range []Organization{
				{ID: "org_alpha", ProviderID: "porg_alpha", Name: "Alpha", Endpoint: testSocketEndpoint("alpha.sock"), Generation: 1},
				{ID: "org_beta", ProviderID: "porg_beta", Name: "Beta", Endpoint: testSocketEndpoint("beta.sock"), Generation: 1},
			} {
				if _, err := service.Registry().Register(t.Context(), organization); err != nil {
					t.Fatal(err)
				}
			}
			var mu sync.Mutex
			var ids []string
			pages := 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				segments := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
				if len(segments) != 3 {
					t.Error("App JWT is missing")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				claims, err := base64.RawURLEncoding.DecodeString(segments[1])
				if err != nil || !strings.Contains(string(claims), `"iss":"123"`) {
					t.Errorf("App JWT issuer: %s, %v", claims, err)
				}
				if r.Method == http.MethodPost {
					id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/hook/deliveries/"), "/attempts")
					ids = append(ids, id)
					if test.postError {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					w.WriteHeader(http.StatusAccepted)
					return
				}
				pages++
				if test.listError {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if r.URL.Path != "/app/hook/deliveries" || r.URL.Query().Get("per_page") != "100" {
					t.Errorf("unexpected API request %s", r.URL)
				}
				var deliveries []githubDelivery
				switch r.URL.Query().Get("cursor") {
				case "":
					w.Header().Set("Link", `</app/hook/deliveries?per_page=100&cursor=next>; rel="next"`)
					deliveries = []githubDelivery{
						{ID: 900, GUID: "after-startup", DeliveredAt: before.Add(time.Second), StatusCode: 503, Event: "issues"},
						{ID: 110, GUID: "success", DeliveredAt: since.Add(5 * time.Minute), StatusCode: 202, Event: "issues"},
						{ID: 120, GUID: "unrelated", DeliveredAt: since.Add(4 * time.Minute), StatusCode: 503, Event: "push"},
						{ID: 101, GUID: "issue", DeliveredAt: since.Add(time.Minute), StatusCode: 503, Event: "issues"},
					}
					if test.duplicate || test.recovered {
						status := 0
						if test.recovered {
							status = 200
						}
						deliveries = append(deliveries, githubDelivery{ID: 103, GUID: "issue", DeliveredAt: since.Add(2 * time.Minute), StatusCode: status, Event: "issues"})
					}
				case "next":
					w.Header().Set("Link", `</app/hook/deliveries?per_page=100&cursor=old>; rel="next"`)
					deliveries = []githubDelivery{
						{ID: 101, GUID: "issue", DeliveredAt: since.Add(time.Minute), StatusCode: 503, Event: "issues"},
						{ID: 102, GUID: "comment", DeliveredAt: since.Add(30 * time.Second), StatusCode: 302, Event: "issue_comment"},
						{ID: 200, GUID: "before-gap", DeliveredAt: since.Add(-time.Second), StatusCode: 503, Event: "issues"},
					}
				default:
					t.Error("replay paginated before the last accepted receipt")
				}
				if err := json.NewEncoder(w).Encode(deliveries); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(api.Close)
			service.config.GitHubApp = &github.InstallationTokenConfig{AppID: "123", PrivateKey: privateKey, Endpoint: api.URL + "/graphql", HTTPClient: api.Client()}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			served := make(chan error, 1)
			go func() { served <- service.serve(ctx, listener) }()
			t.Cleanup(func() {
				cancel()
				if err := <-served; err != nil {
					t.Error(err)
				}
			})
			select {
			case record := <-done:
				var requested int64
				record.Attrs(func(attr slog.Attr) bool {
					if attr.Key == "requested" {
						requested = attr.Value.Int64()
					}
					return true
				})
				if requested != test.wantRequested || (record.Level == slog.LevelWarn) != test.wantWarning {
					t.Fatalf("replay requested = %d, level = %s", requested, record.Level)
				}
			case <-ctx.Done():
				t.Fatal("startup replay did not finish")
			}
			if !test.receiptError {
				slices.Sort(receiptIDs)
				if !slices.Equal(receiptIDs, []string{"org_alpha", "org_beta"}) {
					t.Fatalf("tenant receipt requests = %v", receiptIDs)
				}
			}
			client := &http.Client{Timeout: time.Second}
			for range 3 {
				response, err := client.Get("http://" + listener.Addr().String() + "/")
				if err != nil {
					t.Fatal(err)
				}
				if err := response.Body.Close(); err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != http.StatusOK {
					t.Fatalf("entry unavailable after replay: %d", response.StatusCode)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(ids, test.wantIDs) || pages != test.wantPages {
				t.Fatalf("redelivery requests = %v, pages = %d; want %v, %d", ids, pages, test.wantIDs, test.wantPages)
			}
		})
	}
}
