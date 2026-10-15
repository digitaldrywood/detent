package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDoctorInvariants(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		manifest bool
		failure  bool
	}{
		{"not configured", false, false}, {"passing", true, false}, {"failing", true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if tt.manifest {
				if err := os.Mkdir(filepath.Join(root, "invariants"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "invariants", "policy.json"), []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			checks := checkDoctorInvariants(t.Context(), "example", root, func(_ context.Context, dir string) error {
				called = true
				if dir != root {
					t.Fatalf("directory = %q", dir)
				}
				if tt.failure {
					return errors.New("required test missing")
				}
				return nil
			})
			if called != tt.manifest {
				t.Fatalf("called = %v", called)
			}
			if !tt.manifest {
				if len(checks) != 0 {
					t.Fatal(checks)
				}
				return
			}
			want := doctorOK
			if tt.failure {
				want = doctorFail
			}
			if len(checks) != 1 || checks[0].Status != want {
				t.Fatal(checks)
			}
		})
	}
}
