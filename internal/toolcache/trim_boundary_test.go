package toolcache

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Err runs at the walk callback boundary, making replacement deterministic.
type trimSwapContext struct {
	context.Context //nolint:containedctx // Wrap the caller context to replace a directory at the walk callback boundary.
	swap            func()
}

func (c *trimSwapContext) Err() error {
	if c.swap != nil {
		swap := c.swap
		c.swap = nil
		swap()
	}
	return c.Context.Err()
}

func TestTrimRootReplacement(t *testing.T) {
	for _, tt := range []struct {
		name     string
		age      time.Duration
		maxBytes int64
	}{
		{"age eviction", 72 * time.Hour, 1 << 20},
		{"size eviction", 0, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			root, outside := filepath.Join(base, "cache"), filepath.Join(base, "outside")
			name := filepath.Join("aa", strings.Repeat("a", 64)+"-d")
			now := time.Now()
			for _, dir := range []string{root, outside} {
				if err := os.MkdirAll(filepath.Join(dir, "aa"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "README"), []byte("Go cache"), 0600); err != nil {
					t.Fatal(err)
				}
				file := filepath.Join(dir, name)
				if err := os.WriteFile(file, []byte("untouched"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(file, now.Add(-tt.age), now.Add(-tt.age)); err != nil {
					t.Fatal(err)
				}
			}
			moved := filepath.Join(base, "moved")
			ctx := &trimSwapContext{Context: t.Context(), swap: func() {
				if err := os.Rename(root, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, root); err != nil {
					t.Fatal(err)
				}
			}}
			if _, err := Trim(ctx, root, Policy{MaxAge: 48 * time.Hour, MaxBytes: tt.maxBytes}, now); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(filepath.Join(outside, name)); err != nil || string(data) != "untouched" {
				t.Fatalf("outside file changed: %q, %v", data, err)
			}
			if _, err := os.Stat(filepath.Join(outside, "detent-trim.txt")); !os.IsNotExist(err) {
				t.Fatalf("outside marker created: %v", err)
			}
			if _, err := os.Stat(filepath.Join(moved, name)); !os.IsNotExist(err) {
				t.Fatalf("original cache entry not evicted: %v", err)
			}
		})
	}
}
