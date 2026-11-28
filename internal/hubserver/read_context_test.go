package hubserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRequestDeadline(t *testing.T) {
	const budget = 35 * time.Second
	for _, tc := range []struct {
		name   string
		path   string
		caller time.Duration
		want   time.Duration
	}{
		{name: "server budget", path: "/runtime", want: budget},
		{name: "longer caller deadline", path: "/runtime", caller: time.Minute, want: time.Minute},
		{name: "shorter caller deadline", path: "/runtime", caller: time.Second, want: time.Second},
		{name: "dispatch long poll budget", path: hubapi.NativeBase + "/claims/wait", want: 40 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				if tc.caller > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.caller)
					defer cancel()
				}
				s := Service{config: Config{RequestBudget: budget}}
				e := echo.New()
				e.Use(s.requestDeadline)
				var readCtx context.Context
				e.GET(tc.path, func(c echo.Context) error {
					readCtx = c.Request().Context()
					deadline, ok := readCtx.Deadline()
					if !ok || time.Until(deadline) != tc.want {
						t.Errorf("request deadline = %v, present = %t, want %s from now", deadline, ok, tc.want)
					}
					if got := hubapi.ConnectionContext(readCtx); got != ctx {
						t.Error("connection context lost its caller")
					}
					return c.NoContent(http.StatusOK)
				})
				response := httptest.NewRecorder()
				e.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil).WithContext(ctx))
				if response.Code != http.StatusOK || readCtx == nil {
					t.Fatalf("request did not reach handler, status = %d", response.Code)
				}
				if tc.caller == 0 && readCtx.Err() != context.Canceled {
					t.Errorf("server budget context after request = %v, want canceled", readCtx.Err())
				}
				if ctx.Err() != nil {
					t.Errorf("caller context after request = %v, want active", ctx.Err())
				}
			})
		})
	}
}

func TestSharedEntryRequestDeadline(t *testing.T) {
	const budget = 35 * time.Second
	const runtimePath = "/work-items/:item/runtime"
	const base = "/api/v2/organizations/org_security/projects/prj_example"
	for _, tc := range []struct {
		name, route, target string
		caller, want        time.Duration
	}{
		{name: "server budget", route: runtimePath, target: base + "/work-items/wi_example/runtime", want: budget},
		{name: "longer caller deadline", route: runtimePath, target: base + "/work-items/wi_example/runtime", caller: time.Minute, want: time.Minute},
		{name: "shorter caller deadline", route: runtimePath, target: base + "/work-items/wi_example/runtime", caller: 5 * time.Second, want: 5 * time.Second},
		{name: "dispatch long poll", route: "/claims/wait", target: base + "/claims/wait?wait=30", want: 40 * time.Second},
		{name: "conversation long poll", route: "/conversations/:conversation/controls", target: base + "/conversations/conv_example/controls", want: 40 * time.Second},
		{name: "rewritten path", route: runtimePath, target: "/organizations/org_security" + base + "/work-items/wi_example/runtime", want: budget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started := time.Now()
				ctx := t.Context()
				if tc.caller > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.caller)
					defer cancel()
				}
				key := hostedSharedKey(7)
				cfg := hostedSharedTestConfig(nil, key, 1)
				cfg.RequestBudget, cfg.Clock, cfg.Logger = budget, time.Now, discardLogger()
				if err := cfg.Hosted.SharedEntry.Validate(cfg.Hosted.OrganizationID); err != nil {
					t.Fatal(err)
				}
				var slugCtx context.Context
				cfg.AccessRepository = slugDeadlineRepository{read: func(readCtx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
					slugCtx = readCtx
					if org != "org_security" || workload != corestore.Serving {
						t.Errorf("slug read scope = %s/%v, want org_security/serving", org, workload)
					}
					if deadline, ok := readCtx.Deadline(); !ok || !deadline.Equal(started.Add(tc.want)) {
						t.Errorf("slug read deadline = %v, present = %t, want %s from request start", deadline, ok, tc.want)
					}
					timer := time.NewTimer(2 * time.Second)
					defer timer.Stop()
					select {
					case <-timer.C:
						return read(slugDeadlineReader{})
					case <-readCtx.Done():
						return readCtx.Err()
					}
				}}
				cfg.TenantRepository = cfg.AccessRepository
				e := echo.New()
				s := Service{config: cfg, echo: e, database: &database{}}
				e.Use(s.requestDeadline)
				s.registerRoutes(e)
				e.GET(hubapi.NativeBase+tc.route, func(c echo.Context) error {
					if deadline, ok := c.Request().Context().Deadline(); slugCtx == nil || !ok || !deadline.Equal(started.Add(tc.want)) {
						t.Errorf("handler deadline = %v, present = %t, want %s from request start after slug read", deadline, ok, tc.want)
					}
					return c.NoContent(http.StatusOK)
				})
				f := hostedSharedFixture{key: key}
				claims := f.claims(nil, cloudassert.KindMachine, http.MethodGet, tc.target, nil)
				claims.OrganizationSlug = "example"
				assertion, err := cloudassert.Sign(key, claims)
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodGet, tc.target, nil).WithContext(ctx)
				request.Header.Set(cloudassert.Header, assertion)
				request.Header.Set(echo.HeaderAuthorization, "Bearer example")
				response := httptest.NewRecorder()
				e.ServeHTTP(response, request)
				if response.Code != http.StatusOK {
					t.Fatalf("response status = %d, want 200: %s", response.Code, response.Body)
				}
			})
		})
	}
}

type slugDeadlineRepository struct {
	tenantstore.Repository
	read func(context.Context, tracker.OrganizationID, corestore.Workload, func(tenantstore.Reader) error) error
}

func (r slugDeadlineRepository) ReadTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.read(ctx, org, workload, read)
}

type slugDeadlineReader struct{ tenantstore.Reader }

func (slugDeadlineReader) AccessOrganization(context.Context) (accessstore.Organization, error) {
	return accessstore.Organization{Slug: "example"}, nil
}
