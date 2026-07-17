package web

import (
	"testing"
	"time"
)

func TestReportsOutcomeWindow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		value        string
		wantID       string
		wantDuration time.Duration
		wantBucket   time.Duration
		wantErr      bool
	}{
		{name: "default", wantID: "7d", wantDuration: 7 * 24 * time.Hour, wantBucket: 24 * time.Hour},
		{name: "twenty four hours", value: "24h", wantID: "24h", wantDuration: 24 * time.Hour, wantBucket: time.Hour},
		{name: "seven days", value: "7d", wantID: "7d", wantDuration: 7 * 24 * time.Hour, wantBucket: 24 * time.Hour},
		{name: "thirty days", value: "30d", wantID: "30d", wantDuration: 30 * 24 * time.Hour, wantBucket: 24 * time.Hour},
		{name: "unsupported", value: "90d", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := reportsOutcomeWindow(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("reportsOutcomeWindow(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.ID != tt.wantID || got.Duration != tt.wantDuration || got.Bucket != tt.wantBucket {
				t.Fatalf("reportsOutcomeWindow(%q) = %#v, want %s/%s/%s", tt.value, got, tt.wantID, tt.wantDuration, tt.wantBucket)
			}
		})
	}
}
