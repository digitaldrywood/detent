package github

import (
	"context"
	"net/http"
	"slices"
	"testing"
)

func TestFetchRepositoryLabels(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		method: http.MethodGet,
		path:   "/repos/digitaldrywood/detent/labels?per_page=100",
		body:   `[{"name":"bug"},{"name":" requires-human-review "},{"name":""}]`,
	}})
	c := newGitHubTestConnector(t, server, Config{})
	got, err := c.FetchRepositoryLabels(context.Background(), "digitaldrywood/detent")
	if err != nil {
		t.Fatalf("FetchRepositoryLabels() error = %v", err)
	}
	want := []string{"bug", "requires-human-review"}
	if !slices.Equal(got, want) {
		t.Fatalf("FetchRepositoryLabels() = %#v, want %#v", got, want)
	}
}

func TestFetchRepositoryInfoSurfacesVisibility(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		response       string
		wantPrivate    bool
		wantVisibility string
		wantBranch     string
	}{
		{
			name:           "public",
			response:       `{"id":1,"full_name":"digitaldrywood/detent","html_url":"https://github.com/digitaldrywood/detent","private":false,"visibility":"public","default_branch":"main"}`,
			wantVisibility: "public",
			wantBranch:     "main",
		},
		{
			name:           "private fallback",
			response:       `{"id":1,"full_name":"digitaldrywood/detent","html_url":"https://github.com/digitaldrywood/detent","private":true}`,
			wantPrivate:    true,
			wantVisibility: "private",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server := newGraphQLTestServer(t, []graphqlTestResponse{{
				method: http.MethodGet,
				path:   "/repos/digitaldrywood/detent",
				body:   test.response,
			}})
			c := newGitHubTestConnector(t, server, Config{})
			got, err := c.FetchRepositoryInfo(context.Background(), "digitaldrywood/detent")
			if err != nil {
				t.Fatalf("FetchRepositoryInfo() error = %v", err)
			}
			if got.Private != test.wantPrivate || got.Visibility != test.wantVisibility || got.DefaultBranch != test.wantBranch {
				t.Fatalf("FetchRepositoryInfo() = %#v", got)
			}
		})
	}
}
