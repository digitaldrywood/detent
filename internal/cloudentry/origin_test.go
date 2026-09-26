package cloudentry

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestSameOriginBrowserMutations(t *testing.T) {
	t.Parallel()
	service := &Service{config: Config{PublicURL: "https://hub.example.test"}}
	for _, test := range []struct {
		name, origin, site string
		want               bool
	}{
		{"exact origin", "https://hub.example.test", "same-origin", true},
		{"exact origin without fetch metadata", "https://hub.example.test", "", true},
		{"no-referrer form post", "null", "same-origin", true},
		{"null origin from another site", "null", "cross-site", false},
		{"null origin from a sibling site", "null", "same-site", false},
		{"null origin without fetch metadata", "null", "", false},
		{"other origin", "https://attacker.example.test", "same-origin", false},
		{"missing origin", "", "same-origin", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/organizations", nil)
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.site != "" {
				request.Header.Set("Sec-Fetch-Site", test.site)
			}
			if got := service.sameOrigin(echo.New().NewContext(request, httptest.NewRecorder())); got != test.want {
				t.Fatalf("sameOrigin = %v, want %v", got, test.want)
			}
		})
	}
}
