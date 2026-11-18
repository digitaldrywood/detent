//go:build !windows

package runnerauth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRoutingCacheRejectsPublicPermissions(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "private", "runner.json")
	file, err := Initialize(path, "https://hub.example.test")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := RoutingSnapshot{RunnerID: file.Identity.RunnerID, Revision: 3, Routing: Routing{DisplayName: "Runner", State: "active", CapacityLimit: 2, IsolationTier: "native-trusted"}}
	if err := SaveRoutingCache(path, snapshot); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		path string
		mode os.FileMode
	}{
		{"public file", RoutingCachePath(path), 0o644},
		{"public directory", filepath.Dir(path), 0o755},
	} {
		t.Run(test.name, func(t *testing.T) {
			info, err := os.Stat(test.path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(test.path, test.mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Chmod(test.path, info.Mode().Perm()); err != nil {
					t.Error(err)
				}
			})
			if _, err := LoadRoutingCache(path); err == nil {
				t.Fatal("public routing cache accepted on read")
			}
			if err := SaveRoutingCache(path, snapshot); err == nil {
				t.Fatal("public routing cache accepted for update")
			}
		})
	}
	if err := SaveRoutingCache(path, RoutingSnapshot{RunnerID: NewBinding().RunnerID, Revision: 4, Routing: snapshot.Routing}); err == nil {
		t.Fatal("cache accepted another runner")
	}
	directory := t.TempDir()
	snapshot.Routing.HostServices = []string{"unix:" + directory}
	if err := SaveRoutingCache(path, snapshot); err == nil {
		t.Fatal("cache accepted a local directory as a host service")
	}
	linked := filepath.Join(directory, "linked.sock")
	if err := os.Symlink(filepath.Join(directory, "target.sock"), linked); err != nil {
		t.Fatal(err)
	}
	snapshot.Routing.HostServices = []string{"unix:" + linked}
	if err := SaveRoutingCache(path, snapshot); err == nil {
		t.Fatal("cache accepted a local symbolic link as a host service")
	}
}
