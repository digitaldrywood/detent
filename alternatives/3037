package testenv

import (
	"context"
	"io"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
)

type databaseFixtureCloser func() error

func (f databaseFixtureCloser) Close() error { return f() }

func TestDatabaseTemplateKeepsClosedImageAndPrivateCopies(t *testing.T) {
	t.Parallel()
	var opens atomic.Int64
	pathForTest := DatabaseTemplate(func(_ context.Context, path string) (io.Closer, error) {
		opens.Add(1)
		return databaseFixtureCloser(func() error {
			// The committed image becomes readable only when the opener closes.
			return os.WriteFile(path, []byte("closed image"), 0o600)
		}), nil
	})
	first, second := pathForTest(t), pathForTest(t)
	if first == second || opens.Load() != 1 {
		t.Fatalf("copies=%q,%q opens=%d", first, second, opens.Load())
	}
	if err := os.WriteFile(first, []byte("first test writes"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(second)
	if err != nil || string(data) != "closed image" {
		t.Fatalf("second copy=%q error=%v", data, err)
	}
	third := pathForTest(t)
	data, err = os.ReadFile(third)
	if err != nil || string(data) != "closed image" || opens.Load() != 1 {
		t.Fatalf("template mutated: third=%q opens=%d error=%v", data, opens.Load(), err)
	}
	info, err := os.Stat(second)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("copy permissions=%v", info.Mode().Perm())
	}
}
