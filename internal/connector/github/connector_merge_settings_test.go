package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConnectorRepositoryMergeSettings(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/digitaldrywood/detent" {
			t.Fatalf("request = %s %s, want GET /repos/digitaldrywood/detent", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"allow_merge_commit":false,"allow_squash_merge":true,"allow_rebase_merge":false}`))
	}))
	t.Cleanup(server.Close)

	connector, err := NewConnector(Config{Endpoint: server.URL + "/graphql", APIKey: "token"})
	if err != nil {
		t.Fatalf("NewConnector() error = %v", err)
	}
	got, err := connector.RepositoryMergeSettings(context.Background(), "digitaldrywood/detent")
	if err != nil {
		t.Fatalf("RepositoryMergeSettings() error = %v", err)
	}
	want := RepositoryMergeSettings{AllowSquashMerge: true}
	if got != want {
		t.Fatalf("RepositoryMergeSettings() = %#v, want %#v", got, want)
	}
}
