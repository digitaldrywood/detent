package hubauth_test

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func TestBearerTokenRejectsAmbiguousHeaders(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		headers []string
		want    string
	}{
		{"missing", nil, ""},
		{"duplicate", []string{"Bearer first", "Bearer second"}, ""},
		{"duplicate matching", []string{"Bearer same", "Bearer same"}, ""},
		{"basic", []string{"Basic example"}, ""},
		{"scheme without separator", []string{"Bearer"}, ""},
		{"tab separator", []string{"Bearer\texample"}, ""},
		{"empty", []string{"Bearer "}, ""},
		{"oversized", []string{"Bearer " + strings.Repeat("x", 4097)}, ""},
		{"maximum", []string{"Bearer " + strings.Repeat("x", 4096)}, strings.Repeat("x", 4096)},
		{"case insensitive", []string{"bEaReR example"}, "example"},
		{"trim whitespace", []string{" \tBearer  example \t"}, "example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest("GET", "/", nil)
			for _, value := range tc.headers {
				request.Header.Add(echo.HeaderAuthorization, value)
			}
			c := echo.New().NewContext(request, httptest.NewRecorder())
			token, err := hubauth.BearerToken(c)
			if token != tc.want {
				t.Fatalf("token length=%d want=%d", len(token), len(tc.want))
			}
			if tc.want == "" {
				if !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("error=%v want access denied", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, c := range []echo.Context{nil, echo.New().NewContext(nil, httptest.NewRecorder())} {
		if token, err := hubauth.BearerToken(c); token != "" || !errors.Is(err, operatortool.ErrAccessDenied) {
			t.Fatalf("missing request token length=%d error=%v", len(token), err)
		}
	}
}
