package store

import (
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestInsertsNeverWriteConstantUniqueKeys(t *testing.T) {
	s, err := openSQLite(t.Context(), Config{Path: filepath.Join(t.TempDir(), "detent.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	violations, err := testenv.ConstantUniqueInserts(t.Context(), s.db, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range violations {
		t.Error(violation)
	}
}
