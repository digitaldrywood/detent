package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/scheduleowner"
)

func TestCheckDoctorScheduleOwnership(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		enabled    bool
		status     scheduleowner.Status
		probeErr   error
		wantStatus doctorStatus
		wantDetail string
	}{
		{name: "missing config", wantStatus: doctorFail, wantDetail: "enabled is false"},
		{name: "backend unreachable", enabled: true, probeErr: errors.New("forbidden"), wantStatus: doctorFail, wantDetail: "no reachable schedule owner"},
		{name: "no owner", enabled: true, status: scheduleowner.Status{}, wantStatus: doctorFail, wantDetail: "no active schedule owner"},
		{name: "expired owner", enabled: true, status: scheduleowner.Status{Owner: "alpha", Generation: 2, ExpiresAt: now.Add(-time.Minute)}, wantStatus: doctorFail, wantDetail: "expired"},
		{name: "active owner", enabled: true, status: scheduleowner.Status{Owner: "alpha", Generation: 3, RenewedAt: now, ExpiresAt: now.Add(5 * time.Minute), Active: true}, wantStatus: doctorOK, wantDetail: "owner alpha holds generation 3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := workflowconfig.Default()
			cfg.ScheduleOwnership.Enabled = tt.enabled
			check := checkDoctorScheduleOwnership(t.Context(), "example", cfg, doctorDeps{
				scheduleOwnership: func(context.Context, workflowconfig.Config) (scheduleowner.Status, error) {
					return tt.status, tt.probeErr
				},
			})
			if check.Status != tt.wantStatus || !strings.Contains(check.Detail, tt.wantDetail) {
				t.Fatalf("check = %#v, want status %s detail %q", check, tt.wantStatus, tt.wantDetail)
			}
		})
	}
}
