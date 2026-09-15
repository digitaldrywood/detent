package web_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"

	"github.com/digitaldrywood/detent/internal/update"
	"github.com/digitaldrywood/detent/internal/web"
)

func TestUpdateApplyEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		applier    *updateApplierStub
		form       url.Values
		wantStatus int
		want       string
		wantCalls  int
	}{
		{
			name:       "unavailable",
			form:       url.Values{"confirm": {"true"}},
			wantStatus: http.StatusServiceUnavailable,
			want:       "Update apply is unavailable",
		},
		{
			name:       "confirmation required",
			applier:    &updateApplierStub{},
			wantStatus: http.StatusPreconditionRequired,
			want:       "confirm=true",
		},
		{
			name:       "pending state changed",
			applier:    &updateApplierStub{err: update.ErrNoPendingUpdate},
			form:       url.Values{"confirm": {"true"}},
			wantStatus: http.StatusConflict,
			want:       "No Detent update is pending",
			wantCalls:  1,
		},
		{
			name:       "explicit release",
			applier:    &updateApplierStub{status: update.Status{LatestVersion: "1.2.4"}},
			form:       url.Values{"confirm": {"true"}, "release": {"true"}, "from_release": {"true"}},
			wantStatus: http.StatusAccepted, want: "Detent is restarting", wantCalls: 1,
		},
		{
			name:       "applies update",
			applier:    &updateApplierStub{status: update.Status{LatestVersion: "1.2.4"}},
			form:       url.Values{"confirm": {"true"}},
			wantStatus: http.StatusAccepted,
			want:       "Detent is restarting",
			wantCalls:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			deps := testDeps(t)
			if tt.applier != nil {
				deps.UpdateApplier = tt.applier
			}
			server, err := web.NewServer(web.Config{}, deps)
			if err != nil {
				t.Fatalf("NewServer() error = %v", err)
			}
			recorder := performForm(t, server.Handler(), http.MethodPost, "/api/v1/update/apply", tt.form)
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), tt.want) {
				t.Fatalf("body missing %q: %s", tt.want, recorder.Body.String())
			}
			if tt.applier != nil && tt.applier.calls != tt.wantCalls {
				t.Fatalf("ApplyPending() calls = %d, want %d", tt.applier.calls, tt.wantCalls)
			}
		})
	}
}

func TestUpdateApplyEndpointReportsFailureWithoutDetails(t *testing.T) {
	t.Parallel()

	applier := &updateApplierStub{err: errors.New("download contained fixture secret")}
	deps := testDeps(t)
	deps.UpdateApplier = applier
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	recorder := performForm(t, server.Handler(), http.MethodPost, "/api/v1/update/apply", url.Values{"confirm": {"true"}})
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusInternalServerError, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "fixture secret") || !strings.Contains(recorder.Body.String(), "Detent update apply failed") {
		t.Fatalf("failure response = %s", recorder.Body.String())
	}
}

type updateApplierStub struct {
	status update.Status
	err    error
	calls  int
}

func (s *updateApplierStub) ApplyPending(context.Context) (update.Status, error) {
	s.calls++
	return s.status, s.err
}

func (s *updateApplierStub) ApplyRelease(ctx context.Context, fromRelease bool) (update.Status, error) {
	return s.ApplyPending(ctx)
}

func TestAPIStateReportsUpdateDrain(t *testing.T) {
	t.Parallel()
	for _, count := range []int{0, 2} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			deps := testDeps(t)
			if err := deps.Hub.Publish(telemetry.Snapshot{GeneratedAt: time.Now(), Update: telemetry.Update{State: "draining", ActiveAttempts: count}}); err != nil {
				t.Fatal(err)
			}
			server, err := web.NewServer(web.Config{}, deps)
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/state", nil))
			var got struct {
				Status string
				Update telemetry.Update
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Status != "draining" || got.Update.State != "draining" || got.Update.ActiveAttempts != count {
				t.Fatalf("state = %+v", got)
			}
		})
	}
}
