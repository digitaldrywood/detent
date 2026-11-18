package executionrecords_test

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
)

func TestMain(m *testing.M) {
	pgstoretest.Main(m)
}
