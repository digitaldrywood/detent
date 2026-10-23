package artifact

import (
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestInsertsNeverWriteConstantUniqueKeys(t *testing.T) {
	c, err := openCatalog(t.Context(), filepath.Join(t.TempDir(), "artifacts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.db.Close(); err != nil {
			t.Error(err)
		}
	})
	violations, err := testenv.ConstantUniqueInserts(t.Context(), c.db, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range violations {
		t.Error(violation)
	}
}
