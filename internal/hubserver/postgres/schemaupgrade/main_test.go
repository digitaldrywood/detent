package schemaupgrade_test

import (
	"flag"
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
)

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(0)
	}
	pgstoretest.Main(m)
}
