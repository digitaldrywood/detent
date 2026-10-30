package hubserver

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestInsertsNeverWriteConstantUniqueKeys(t *testing.T) {
	db, err := openDatabase(t.Context(), hostedStorageConfig(t).normalized())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, directory := range []string{".", "../hubconversation"} {
		violations, err := testenv.ConstantUniqueInserts(t.Context(), db.db, directory)
		if err != nil {
			t.Fatal(err)
		}
		for _, violation := range violations {
			t.Error(violation)
		}
	}
}
