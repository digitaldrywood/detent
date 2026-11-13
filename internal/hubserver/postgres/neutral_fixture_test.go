package postgres_test

import (
	"os"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

var neutralFixtureCatalog = sync.OnceValues(func() (storagecatalog.Catalog, error) {
	return storagecatalog.Read(os.DirFS("../../.."), "tools/storageinventory/mappings/v1.json")
})

func neutralFixtureTable(t *testing.T, name string, keys []neutralimport.Key) neutralimport.Table {
	t.Helper()
	catalog, err := neutralFixtureCatalog()
	if err != nil {
		t.Fatal(err)
	}
	return neutralimporttest.Table(t, catalog, name, keys)
}
