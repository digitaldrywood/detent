package workspacerunner

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestStartRunAdmitsOnlyUnderAValidLease(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		stale    bool
		closing  bool
		validFor time.Duration
		wantCode string
	}{
		{name: "a held lease admits the run", validFor: time.Minute},
		{name: "a lost lease refuses it", stale: true, validFor: time.Minute, wantCode: workspacesession.CodeStaleExecution},
		{name: "a closing workspace refuses it", closing: true, validFor: time.Minute, wantCode: workspacesession.CodeStaleExecution},
		{name: "an expired lease window refuses it", validFor: -time.Second, wantCode: workspacesession.CodeStaleExecution},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			s := &Session{
				config:          Config{Now: func() time.Time { return now }},
				runs:            map[string]*execRun{},
				stale:           test.stale,
				closing:         test.closing,
				leaseValidUntil: now.Add(test.validFor),
			}
			_, code, _ := s.startRun(context.Background(), "conn:1")
			if code != test.wantCode {
				t.Fatalf("startRun() code = %q, want %q", code, test.wantCode)
			}
			_, registered := s.runs["conn:1"]
			if registered != (test.wantCode == "") {
				t.Fatalf("run registered = %t, want %t", registered, test.wantCode == "")
			}
			if registered {
				s.finishRun("conn:1")
			}
			s.running.Wait()
		})
	}
}
