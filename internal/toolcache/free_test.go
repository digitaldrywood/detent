//go:build darwin || linux || windows

package toolcache

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFreeBytes(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing", true: "uncreated cache"}[missing], func(t *testing.T) {
			root := t.TempDir()
			if missing {
				root = filepath.Join(root, "not-created", "go-build")
			}
			if _, err := FreeBytes(root); err != nil {
				t.Fatal(err)
			}
			if missing {
				if _, err := os.Stat(root); !os.IsNotExist(err) {
					t.Fatalf("inspection created cache: %v", err)
				}
			}
		})
	}
}
