package codex

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestHostCacheWritableRoots(t *testing.T) {
	root := t.TempDir()
	build := filepath.Join(root, "build")
	modules := filepath.Join(root, "modules")
	t.Setenv("GOCACHE", build)
	t.Setenv("GOMODCACHE", modules)
	for _, tt := range []struct {
		name, policy         string
		restricted, writable bool
	}{
		{"workspace write", "workspace-write", false, true},
		{"full access", "danger-full-access", false, false},
		{"read only", "read-only", false, false},
		{"restricted worker", "workspace-write", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := hostCacheWritableRoots(context.Background(), Options{ThreadSandbox: tt.policy}, []string{"/existing"}, tt.restricted)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"/existing"}
			if tt.writable {
				want = append(want, build, modules)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("roots=%v want=%v", got, want)
			}
		})
	}
}
