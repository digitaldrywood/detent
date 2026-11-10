package hubserver

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestInsertsNeverWriteConstantUniqueKeys(t *testing.T) {
	db := postgresStartupTemplate.Database(t)
	for _, dir := range []string{".", "postgres"} {
		t.Run(dir, func(t *testing.T) {
			violations, err := testenv.ConstantUniquePostgresInserts(t.Context(), db, dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, violation := range violations {
				t.Error(violation)
			}
		})
	}
}
