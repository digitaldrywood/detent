package testenv

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// DatabaseTemplate migrates an empty database once, closes it, and gives each
// test its own private copy. The opener must not seed test-specific identities.
func DatabaseTemplate(open func(context.Context, string) (io.Closer, error)) func(testing.TB) string {
	image := sync.OnceValues(func() (_ []byte, resultErr error) {
		dir, err := os.MkdirTemp("", "detent-database-fixture-")
		if err != nil {
			return nil, err
		}
		defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(dir)) }()
		path := filepath.Join(dir, "database.db")
		backend, err := open(context.Background(), path)
		if err != nil {
			return nil, err
		}
		if err := backend.Close(); err != nil {
			return nil, err
		}
		return os.ReadFile(path)
	})
	return func(t testing.TB) string {
		t.Helper()
		data, err := image()
		if err != nil {
			t.Fatalf("build migrated database fixture: %v", err)
		}
		path := filepath.Join(t.TempDir(), "database.db")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write migrated database fixture: %v", err)
		}
		return path
	}
}
