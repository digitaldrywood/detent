package web_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

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
