package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestDoctorLiveReachability(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, status, mode, body, unavailable string
		listenErr                             error
		code                                  int
		err                                   error
	}{
		{name: "healthy", status: "ok", mode: "running"},
		{name: "remote unhealthy", listenErr: syscall.EADDRNOTAVAIL, status: "needs_attention", mode: "running"},
		{name: "unhealthy", status: "needs_attention", mode: "running"},
		{name: "stopped", err: syscall.ECONNREFUSED, unavailable: "could not be reached"},
		{name: "unrelated", body: `{"status":"ok"}`, unavailable: "did not return Detent health"},
		{name: "unauthorized", code: http.StatusUnauthorized, unavailable: "HTTP 401"},
		{name: "timeout", err: context.DeadlineExceeded, unavailable: "deadline exceeded"},
		{name: "incompatible", body: `{"status":`, unavailable: "did not return Detent health"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			deps := doctorDeps{httpDo: func(*http.Request) (*http.Response, error) {
				calls++
				if tt.err != nil {
					return nil, tt.err
				}
				body := tt.body
				if body == "" {
					body = fmt.Sprintf(`{"status":%q,"mode":%q,"ready":true,"lifecycle":"ready","version":"v1","commit":"abc","checks":{"hub":"configured","store":"configured","registry":"configured","connector":"configured"},"stranded_active_issues":[{"project_id":"detent","identifier":"detent#1"}],"staleness_warnings":[{"project_id":"detent","class":"fault","identifier":"detent#2","reason":"stalled"}]}`, tt.status, tt.mode)
				}
				code := tt.code
				if code == 0 {
					code = http.StatusOK
				}
				return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body))}, nil
			}, listen: func(string, string) (net.Listener, error) {
				if tt.listenErr != nil {
					return nil, tt.listenErr
				}
				return nil, syscall.EADDRINUSE
			}}.withDefaults()
			boot := BootConfig{}
			stranded := checkDoctorStrandedActive(t.Context(), boot, "", deps)
			dispatch := checkDoctorDispatchStalls(t.Context(), boot, "", deps)
			notifications := checkDoctorHealthNotificationDelivery(t.Context(), boot, "", deps)
			service := checkDoctorDetentService(t.Context(), boot, buildinfo.Info{}, deps)
			stale := checkDoctorFleetStaleness(t.Context(), boot, "", deps)
			drift := checkDoctorWorkflowDrift(t.Context(), globalconfig.Config{Projects: []globalconfig.Project{{ID: "detent"}}}, boot, deps)
			port := checkDoctorServerPort(t.Context(), boot, deps)
			if calls != 7 {
				t.Fatalf("health calls = %d, want 7", calls)
			}
			if tt.unavailable != "" {
				for _, check := range []doctorCheck{stranded, stale, drift[0], dispatch, notifications} {
					if check.Status != doctorWarn || !strings.Contains(check.Detail, tt.unavailable) {
						t.Errorf("unavailable evidence = %+v, want warning containing %q", check, tt.unavailable)
					}
				}
				if port.Status != doctorFail {
					t.Errorf("port = %+v, want failure", port)
				}
				return
			}
			if tt.status == "needs_attention" && (len(service) == 0 || service[0].Status != doctorWarn || !strings.Contains(service[0].Detail, tt.status)) {
				t.Errorf("unhealthy service warning missing: %+v", service)
			}
			if len(stranded.StrandedIssues) != 1 || len(stale.StalenessWarnings) != 1 {
				t.Errorf("live findings suppressed: stranded=%+v stale=%+v", stranded, stale)
			}
			if len(drift) != 1 || drift[0].Name != "Project detent workflow runtime" || drift[0].Status == doctorOK {
				t.Errorf("runtime comparison did not execute: %+v", drift)
			}
			if port.Status != doctorOK || strings.Contains(port.Detail, "occupied") {
				t.Errorf("running listener = %+v, want OK without startup conflict", port)
			}
		})
	}
}
