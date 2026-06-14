package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

func TestConnectorAuthenticateValidatesViewerAndProject(t *testing.T) {
	t.Parallel()

	requests := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}
		requests <- payload

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"viewer":{"login":"octocat"},"node":{"__typename":"ProjectV2","id":"PVT_1"}}}`))
	}))
	t.Cleanup(server.Close)

	c, err := NewConnector(Config{
		Endpoint:    server.URL,
		APIKey:      "token",
		ProjectSlug: "PVT_1",
		HTTPClient:  server.Client(),
		GHToken: func(context.Context, string) (string, error) {
			t.Fatal("gh token fallback should not run")
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("NewConnector() error = %v", err)
	}

	if err := c.Authenticate(context.Background()); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if got := c.InstanceLogin(); got != "octocat" {
		t.Fatalf("InstanceLogin() = %q, want octocat", got)
	}

	payload := <-requests
	variables := payload["variables"].(map[string]any)
	if variables["projectId"] != "PVT_1" {
		t.Fatalf("projectId = %v, want PVT_1", variables["projectId"])
	}
}

func TestConnectorInstanceLoginConcurrentAuthenticateAndRead(t *testing.T) {
	t.Parallel()

	var count atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		login := "octocat-" + strconv.FormatInt(count.Add(1), 10)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"viewer":{"login":"` + login + `"},"node":{"__typename":"ProjectV2","id":"PVT_1"}}}`))
	}))
	t.Cleanup(server.Close)

	c, err := NewConnector(Config{
		Endpoint:    server.URL,
		APIKey:      "token",
		ProjectSlug: "PVT_1",
		HTTPClient:  server.Client(),
		GHToken: func(context.Context, string) (string, error) {
			t.Fatal("gh token fallback should not run")
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("NewConnector() error = %v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			<-start
			for range 25 {
				if err := c.Authenticate(context.Background()); err != nil {
					t.Errorf("Authenticate() error = %v", err)
				}
			}
		})
	}
	wg.Go(func() {
		<-start
		for range 200 {
			_ = c.InstanceLogin()
		}
	})

	close(start)
	wg.Wait()
}

func TestConnectorAuthenticateReportsProjectProblems(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		project string
		body    string
		want    error
	}{
		{
			name:    "missing project",
			project: "",
			want:    ErrMissingProject,
		},
		{
			name:    "project not found",
			project: "PVT_missing",
			body:    `{"data":{"viewer":{"login":"octocat"},"node":null}}`,
			want:    ErrProjectNotFound,
		},
		{
			name:    "wrong node type",
			project: "I_kw1",
			body:    `{"data":{"viewer":{"login":"octocat"},"node":{"__typename":"Issue","id":"I_kw1"}}}`,
			want:    ErrProjectNotFound,
		},
		{
			name:    "viewer missing",
			project: "PVT_1",
			body:    `{"data":{"viewer":null,"node":{"__typename":"ProjectV2","id":"PVT_1"}}}`,
			want:    ErrAuthenticationFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(server.Close)

			c, err := NewConnector(Config{
				Endpoint:    server.URL,
				APIKey:      "token",
				ProjectSlug: tt.project,
				HTTPClient:  server.Client(),
				GHToken: func(context.Context, string) (string, error) {
					t.Fatal("gh token fallback should not run")
					return "", nil
				},
			})
			if err != nil {
				t.Fatalf("NewConnector() error = %v", err)
			}

			err = c.Authenticate(context.Background())
			if !errors.Is(err, tt.want) {
				t.Fatalf("Authenticate() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestConnectorReportsMissingProjectForProjectOperations(t *testing.T) {
	t.Parallel()

	c, err := NewConnector(Config{APIKey: "token"})
	if err != nil {
		t.Fatalf("NewConnector() error = %v", err)
	}

	if _, err := c.FetchCandidateIssues(context.Background()); !errors.Is(err, ErrMissingProject) {
		t.Fatalf("FetchCandidateIssues() error = %v, want ErrMissingProject", err)
	}
	if _, err := c.FetchIssuesByStates(context.Background(), []string{"Todo"}); !errors.Is(err, ErrMissingProject) {
		t.Fatalf("FetchIssuesByStates() error = %v, want ErrMissingProject", err)
	}
	if _, err := c.FetchIssueStatesByIDs(context.Background(), []string{"I_kw1"}); !errors.Is(err, ErrMissingProject) {
		t.Fatalf("FetchIssueStatesByIDs() error = %v, want ErrMissingProject", err)
	}
	if err := c.UpdateIssueState(context.Background(), "I_kw1", "Done"); !errors.Is(err, ErrMissingProject) {
		t.Fatalf("UpdateIssueState() error = %v, want ErrMissingProject", err)
	}
}
