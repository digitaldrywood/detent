package toolcache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestINV12TrimReadout(t *testing.T) {
	for _, tt := range []struct {
		name, marker    string
		trim, wantError bool
	}{
		{"never trimmed", "", false, false},
		{"completed trim", "", true, false},
		{"legacy timestamp", "2026-09-14T12:00:00Z", false, false},
		{"invalid counters", "2026-09-14T12:00:00Z\ninvalid", false, true},
		{"invalid timestamp", "invalid", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "README"), []byte("Go cache"), 0o600); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "detent-trim.txt")
			if tt.marker != "" {
				if err := os.WriteFile(marker, []byte(tt.marker), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
			if tt.trim {
				if _, err := Trim(t.Context(), root, Policy{}, now); err != nil {
					t.Fatal(err)
				}
			}
			got := inspect(t.Context(), func(context.Context) (Paths, error) { return Paths{Build: root, Modules: t.TempDir()}, nil })
			if (got.Error != "") != tt.wantError {
				t.Fatalf("report = %+v", got)
			}
			want := ""
			if tt.trim || tt.name == "legacy timestamp" {
				want = now.Format(time.RFC3339Nano)
			}
			if got.LastTrim != want {
				t.Fatalf("last trim = %q, want %q", got.LastTrim, want)
			}
			if !tt.trim && tt.marker == "" {
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatalf("inspection created metadata: %v", err)
				}
			}
		})
	}
}
