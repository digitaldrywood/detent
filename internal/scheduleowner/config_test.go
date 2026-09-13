package scheduleowner

import "testing"

func TestNormalizedDefaultCadence(t *testing.T) {
	t.Parallel()
	config := (Config{}).Normalized("example/coordination", "")
	tests := []struct {
		name string
		got  int
		want int
	}{
		{name: "lease", got: config.LeaseSeconds, want: 900},
		{name: "heartbeat", got: config.HeartbeatSeconds, want: 300},
		{name: "retry", got: config.RetrySeconds, want: 120},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.got != tt.want {
				t.Fatalf("default %s seconds = %d, want %d", tt.name, tt.got, tt.want)
			}
		})
	}
}
