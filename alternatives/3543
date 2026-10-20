package workspace

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestScratchProcessInspectionError(t *testing.T) {
	inspectionErr := errors.New("cannot read process PEB")
	observationErr := errors.New("cannot observe process")
	for _, tt := range []struct {
		name           string
		exitAfter      time.Duration
		observationErr error
		deadline       time.Duration
		wantErr        bool
	}{
		{name: "already exited"},
		{name: "exits during teardown", exitAfter: 20 * time.Millisecond},
		{name: "live inspection defect", exitAfter: time.Hour, wantErr: true},
		{name: "unverifiable exit", observationErr: observationErr, wantErr: true},
		{name: "cleanup deadline", exitAfter: time.Hour, deadline: 5 * time.Millisecond, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				if tt.deadline != 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tt.deadline)
					defer cancel()
				}
				started := time.Now()
				err := scratchProcessInspectionError(ctx, 2336, "directory", inspectionErr, func(context.Context) (bool, error) {
					return time.Since(started) < tt.exitAfter, tt.observationErr
				})
				if (err != nil) != tt.wantErr {
					t.Fatalf("inspection error = %v, want error %t", err, tt.wantErr)
				}
				if tt.wantErr && (!errors.Is(err, inspectionErr) || !strings.Contains(err.Error(), "directory for process 2336")) {
					t.Fatalf("inspection diagnostic lost: %v", err)
				}
				if tt.observationErr != nil && !errors.Is(err, tt.observationErr) {
					t.Fatalf("observation diagnostic lost: %v", err)
				}
				if got := errors.Is(err, context.DeadlineExceeded); got != (tt.deadline != 0) {
					t.Fatalf("caller deadline error = %t, error = %v", got, err)
				}
				limit := 100 * time.Millisecond
				if tt.deadline != 0 {
					limit = tt.deadline
				}
				if elapsed := time.Since(started); elapsed > limit {
					t.Fatalf("exit confirmation took %v, limit %v", elapsed, limit)
				}
			})
		})
	}
}
