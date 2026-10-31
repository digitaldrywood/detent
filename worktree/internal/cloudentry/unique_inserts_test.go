package cloudentry

import (
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestInsertsNeverWriteConstantUniqueKeys(t *testing.T) {
	for _, database := range []struct {
		name          string
		applicationID int64
	}{
		{"registry", registryApplicationID},
		{"auth", authApplicationID},
	} {
		t.Run(database.name, func(t *testing.T) {
			s, err := openStore(t.Context(), filepath.Join(t.TempDir(), database.name+".db"), database.applicationID, database.name)
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
		})
	}
}
