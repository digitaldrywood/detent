package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestRefreshScanCredentials(t *testing.T) {
	for _, tt := range []struct {
		name        string
		credentials int
	}{{"seven projects one credential", 1}, {"two credentials", 2}} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var mu sync.Mutex
			active := map[string]int{}
			maximum := map[string]int{}
			scans := map[string]string{}
			entered := make(chan struct{}, 14)
			unblock := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				credential := r.Header.Get("Authorization")
				if r.Method != http.MethodPost {
					fmt.Fprint(w, `[]`)
					return
				}
				var request struct {
					Query     string
					Variables map[string]any
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				project := r.Header.Get("X-Project")
				mu.Lock()
				active[credential]++
				maximum[credential] = max(maximum[credential], active[credential])
				if previous := scans[credential]; previous != "" && previous != project {
					t.Errorf("interleaved scan: %s and %s", previous, project)
				}
				scans[credential] = project
				mu.Unlock()
				entered <- struct{}{}
				select {
				case <-unblock:
				case <-ctx.Done():
				}
				mu.Lock()
				active[credential]--
				mu.Unlock()
				var response map[string]any
				if strings.Contains(request.Query, "CandidateHydration") {
					response = map[string]any{"data": map[string]any{"issue0": map[string]any{"id": "I_1000", "comments": map[string]any{"totalCount": 0}, "blockedBy": map[string]any{"nodes": []any{}}}}}
				} else {
					body := projectItemsPageResponseWithTotal(1, false, "", projectIssueNodes(1, "Todo"))
					if err := json.Unmarshal([]byte(body), &response); err != nil {
						t.Error(err)
						return
					}
				}
				addHydratedProjectFields(response["data"].(map[string]any), request.Variables)
				response["data"].(map[string]any)["rateLimit"] = map[string]any{"cost": 3}
				if err := json.NewEncoder(w).Encode(response); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			var wg sync.WaitGroup
			for project := range 7 {
				c, err := NewConnector(Config{Endpoint: server.URL, TokenSource: StaticTokenSource(fmt.Sprintf("token-%d", project%tt.credentials)), HTTPClient: scanProjectClient{server.Client(), strconv.Itoa(project)}, ProjectSlug: fmt.Sprintf("PVT_%d", project), Repository: "digitaldrywood/detent", ActiveStates: []string{"Todo"}})
				if err != nil {
					t.Fatal(err)
				}
				wg.Go(func() {
					release, err := c.BeginRefreshScan(ctx)
					if err != nil {
						t.Error(err)
						return
					}
					defer release()
					scanCtx, points := connector.WithGraphQLPoints(ctx)
					result := c.FetchRefreshIssues(scanCtx, []string{"Todo"}, nil, connector.IssueFilterHint{})
					if result.CandidateError != nil || result.StatusError != nil || len(result.Candidates) != 1 {
						t.Errorf("refresh = %+v", result)
					}
					if got := points.Total(); got != 6 {
						t.Errorf("points = %d, want 6", got)
					}
					mu.Lock()
					delete(scans, fmt.Sprintf("Bearer token-%d", project%tt.credentials))
					mu.Unlock()
				})
			}
			// Independent credentials must both reach the server before either is released.
			for range tt.credentials {
				select {
				case <-entered:
				case <-ctx.Done():
					t.Error("independent scans did not start")
				}
			}
			close(unblock)
			wg.Wait()
			for credential, count := range maximum {
				if count != 1 {
					t.Errorf("%s concurrent requests = %d, want 1", credential, count)
				}
			}
		})
	}
}

func TestRefreshScanCancellation(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(strconv.FormatBool(queued), func(t *testing.T) {
			client, err := NewClient(ClientConfig{Endpoint: "https://scan-cancellation.invalid/graphql", TokenSource: StaticTokenSource(t.Name())})
			if err != nil {
				t.Fatal(err)
			}
			c := &Connector{client: client}
			if queued {
				release, err := c.BeginRefreshScan(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if release, err := c.BeginRefreshScan(ctx); !errors.Is(err, context.Canceled) {
				if release != nil {
					release()
				}
				t.Fatalf("error = %v", err)
			}
		})
	}
}

type scanProjectClient struct {
	client  *http.Client
	project string
}

func (c scanProjectClient) Do(r *http.Request) (*http.Response, error) {
	r.Header.Set("X-Project", c.project)
	return c.client.Do(r)
}
