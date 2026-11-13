package hubauth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func TestResolveNativeRefusesReporterWritesOutsideReporterActions(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		method, path string
		refused      bool
	}{
		{http.MethodPost, hubauth.NativeBase + "/work-items/:item/dependencies", true},
		{http.MethodPost, hubauth.NativeBase + "/work-items/:item/restore", true},
		{http.MethodPost, hubauth.NativeBase + "/work-items/:item/source-recovery", true},
		{http.MethodDelete, hubauth.NativeBase + "/attachment-metadata/:attachment", true},
		{http.MethodPatch, hubauth.NativeBase + "/work-items/:item", false},
		{http.MethodPost, hubauth.NativeBase + "/work-items/:item/comments", false},
		{http.MethodGet, hubauth.NativeBase + "/work-items/:item", false},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			t.Parallel()
			e := echo.New()
			c := e.NewContext(httptest.NewRequest(test.method, "/", nil), httptest.NewRecorder())
			c.SetPath(test.path)
			c.SetParamNames("organization", "project", "item", "attachment")
			c.SetParamValues("org_example", "prj_example", "wi_example", "att_example")
			read := false
			options := hubauth.NativeOptions{
				Credential:      func(echo.Context) (hubauth.Credential, bool) { return hubauth.Credential{ID: "credential_example", HostedRole: "reporter"}, true },
				EventScope:      func(echo.Context, hubauth.Input) bool { return false },
				ArtifactRead:    func(echo.Context) bool { return false },
				RelayTicket:     func(echo.Context) bool { return false },
				LeaseRoute:      func(string) bool { return false },
				RepositoryRoute: func(string) bool { return false },
				ReadProject: func(context.Context, hubauth.Input, bool, bool) error {
					read = true
					return nil
				},
				ReadItem: func(context.Context, hubauth.Input, string) error { return nil },
				Store:    func(echo.Context, hubauth.Input) {},
			}
			err := hubauth.New(hubauth.Deps{}).ResolveNative(t.Context(), c, options)
			if test.refused {
				if !errors.Is(err, operatortool.ErrAccessDenied) || read {
					t.Fatalf("reporter write = %v after project read %t, want operator access refusal before it", err, read)
				}
				return
			}
			if err != nil || !read {
				t.Fatalf("reporter request = %v after project read %t, want project authorization", err, read)
			}
		})
	}
}
