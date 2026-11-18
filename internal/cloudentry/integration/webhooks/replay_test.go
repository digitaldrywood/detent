package webhooks_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudentry"
	"github.com/digitaldrywood/detent/internal/cloudentry/internal/entryfixture"
	"github.com/digitaldrywood/detent/internal/cloudentry/internal/entrytest"
	"github.com/digitaldrywood/detent/internal/cloudentry/internal/harness"
	"github.com/digitaldrywood/detent/internal/connector/github"
)

type githubDelivery struct {
	ID          int64     `json:"id"`
	GUID        string    `json:"guid"`
	DeliveredAt time.Time `json:"delivered_at"`
	StatusCode  int       `json:"status_code"`
	Event       string    `json:"event"`
}

type replayTestHandler struct {
	slog.Handler
	done    chan slog.Record
	address chan string
}

func (h replayTestHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h replayTestHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message == "shared entry serving" {
		record.Attrs(func(a slog.Attr) bool {
			if a.Key == "address" {
				h.address <- a.Value.String()
			}
			return true
		})
	}
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
			address := make(chan string, 1)
			var receiptIDs []string
			cfg := cloudentry.Config{
				Platform:  cloudentry.PlatformConfig{BootstrapAdminEmail: "bootstrap@example.test"},
				PublicURL: harness.PublicURL, ListenAddress: "127.0.0.1:0", Issuer: "entry",
				SigningKey: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)), Provider: harness.NewFakeProvider(),
				StateDir: t.TempDir(), ClientFS: fstest.MapFS{},
				Logger:              slog.New(replayTestHandler{Handler: slog.DiscardHandler, done: done, address: address}),
				GitHubWebhookSecret: []byte("secret"), Now: func() time.Time { return before },
				Transport: func(organization cloudentry.Organization) (http.RoundTripper, error) {
					return entrytest.HandlerTransport{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if strings.HasPrefix(r.URL.Path, "/internal/v1/attachments/") {
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
						if r.URL.Path == "/internal/v1/github/repositories" {
							_, err := io.WriteString(w, "[]")
							if err != nil {
								t.Error(err)
							}
							return
						}
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
			}
			entryfixture.PostgresRestartConfig(t)(&cfg)
			service, err := cloudentry.Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if service != nil {
					if err := service.Close(); err != nil {
						t.Error(err)
					}
				}
			})
			for _, organization := range []cloudentry.Organization{
				{ID: "org_alpha", ProviderID: "porg_alpha", Name: "Alpha", Endpoint: harness.SocketEndpoint("alpha.sock"), Generation: 1},
				{ID: "org_beta", ProviderID: "porg_beta", Name: "Beta", Endpoint: harness.SocketEndpoint("beta.sock"), Generation: 1},
			} {
				if _, err := service.Registry().Register(t.Context(), organization); err != nil {
					t.Fatal(err)
				}
			}
			if err := service.Close(); err != nil {
				t.Fatal(err)
			}
			service = nil
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
			cfg.GitHubApp = &github.InstallationTokenConfig{AppID: "123", PrivateKey: privateKey, Endpoint: api.URL + "/graphql", HTTPClient: api.Client()}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var serveErr error
			served := make(chan struct{})
			go func() {
				defer close(served)
				serveErr = cloudentry.Run(ctx, cfg)
			}()
			t.Cleanup(func() {
				cancel()
				<-served
				if serveErr != nil {
					t.Error(serveErr)
				}
			})
			var listenAddress string
			select {
			case listenAddress = <-address:
			case <-served:
				t.Fatalf("entry stopped before serving: %v", serveErr)
			}
			var record slog.Record
			select {
			case record = <-done:
			case <-served:
				t.Fatalf("entry stopped before replay: %v", serveErr)
			}
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
			if !test.receiptError {
				slices.Sort(receiptIDs)
				if !slices.Equal(receiptIDs, []string{"org_alpha", "org_beta"}) {
					t.Fatalf("tenant receipt requests = %v", receiptIDs)
				}
			}
			client := &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
			t.Cleanup(client.CloseIdleConnections)
			for range 3 {
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+listenAddress+"/", nil)
				if err != nil {
					t.Fatal(err)
				}
				response, err := client.Do(request)
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

func TestMain(m *testing.M) { entryfixture.Main(m) }
