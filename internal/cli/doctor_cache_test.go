package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestDoctorSharedCache(t *testing.T) {
	for _, tt := range []struct {
		name string
		size int64
		want doctorStatus
	}{
		{"empty", 0, doctorOK}, {"small", 4, doctorOK}, {"over budget", workspace.SharedBuildCacheBudget + 1, doctorWarn},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(workspace.SharedCacheRoot(root, "test"), "go-build")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			f, err := os.Create(filepath.Join(dir, "data"))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Truncate(tt.size); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			got := checkDoctorSharedCache(t.Context(), "test", root)
			if got.Status != tt.want || !strings.Contains(got.Detail, "go-build=") {
				t.Fatalf("check = %+v", got)
			}
		})
	}
}
