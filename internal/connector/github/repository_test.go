package github

import (
	"context"
	"net/http"
	"testing"
)

func TestFetchRepositoryInfoSurfacesVisibility(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		response       string
		wantPrivate    bool
		wantVisibility string
	}{
		{
			name:           "public",
			response:       `{"id":1,"full_name":"digitaldrywood/detent","html_url":"https://github.com/digitaldrywood/detent","private":false,"visibility":"public"}`,
			wantVisibility: "public",
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
			if got.Private != test.wantPrivate || got.Visibility != test.wantVisibility {
				t.Fatalf("FetchRepositoryInfo() = %#v", got)
			}
		})
	}
}
