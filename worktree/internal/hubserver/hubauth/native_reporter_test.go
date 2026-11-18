package hubauth_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func TestResolveNativeRefusesReporterWritesOutsideReporterActions(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		method, path    string
		refused, hidden bool
	}{
		{http.MethodPost, hubauth.NativeBase + "/work-items/:item/dependencies", true, false},
		{http.MethodPost, hubauth.NativeBase + "/work-items/:item/restore", true, false},
		{http.MethodPost, hubauth.NativeBase + "/work-items/:item/source-recovery", true, false},
		{http.MethodDelete, hubauth.NativeBase + "/attachment-metadata/:attachment", true, false},
		{http.MethodPost, hubauth.NativeBase + "/work-items/:item/dependencies", true, true},
		{http.MethodPut, hubauth.NativeBase + "/onboarding", true, true},
		{http.MethodPatch, hubauth.NativeBase + "/work-items/:item", false, false},
		{http.MethodPost, hubauth.NativeBase + "/work-items/:item/comments", false, false},
		{http.MethodGet, hubauth.NativeBase + "/work-items/:item", false, false},
	} {
		t.Run(fmt.Sprintf("%s %s hidden=%t", test.method, test.path, test.hidden), func(t *testing.T) {
			t.Parallel()
			e := echo.New()
			c := e.NewContext(httptest.NewRequest(test.method, "/", nil), httptest.NewRecorder())
			c.SetPath(test.path)
			c.SetParamNames("organization", "project", "item", "attachment")
			c.SetParamValues("org_example", "prj_example", "wi_example", "att_example")
			read, authorized := false, false
			options := hubauth.NativeOptions{
				Credential: func(echo.Context) (hubauth.Credential, bool) {
					return hubauth.Credential{ID: "credential_example", HostedRole: "reporter"}, true
				},
				EventScope:      func(echo.Context, hubauth.Input) bool { return false },
				ArtifactRead:    func(echo.Context) bool { return false },
				RelayTicket:     func(echo.Context) bool { return false },
				LeaseRoute:      func(string) bool { return false },
				RepositoryRoute: func(string) bool { return false },
				ReadProject: func(_ context.Context, _ hubauth.Input, write, _ bool) error {
					read, authorized = true, authorized || write
					if test.hidden {
						return hubapi.NotFound()
					}
					return nil
				},
				ReadItem: func(context.Context, hubauth.Input, string) error { return nil },
				Store:    func(echo.Context, hubauth.Input) {},
			}
			err := hubauth.New(hubauth.Deps{}).ResolveNative(t.Context(), c, options)
			if test.refused && test.hidden {
				var missing *hubapi.Error
				if !errors.As(err, &missing) || missing.Status != http.StatusNotFound || authorized {
					t.Fatalf("reporter write on hidden project = %v after write authorization %t, want not found before it", err, authorized)
				}
				return
			}
			if test.refused {
				if !errors.Is(err, operatortool.ErrAccessDenied) || authorized {
					t.Fatalf("reporter write = %v after write authorization %t, want operator access refusal before it", err, authorized)
				}
				return
			}
			if err != nil || !read {
				t.Fatalf("reporter request = %v after project read %t, want project authorization", err, read)
			}
		})
	}
}
