package procstart

import (
	"errors"
	"testing"
)

func TestZombieLeaderStartTime(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stat  string
		start string
		err   error
	}{
		{"another thread remains", "123 (fixture) Z 1 123 123 0 -1 4228108 2050 0 0 0 0 0 0 0 32 12 2 0 437574220", "437574220", nil},
		{"last thread exited", "123 (fixture) Z 1 123 123 0 -1 4228108 2050 0 0 0 0 0 0 0 32 12 1 0 437574220", "", ErrNotRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, err := statStartTime(tc.stat)
			if start != tc.start || !errors.Is(err, tc.err) {
				t.Fatalf("start = %q, error = %v, want %q, %v", start, err, tc.start, tc.err)
			}
		})
	}
}
