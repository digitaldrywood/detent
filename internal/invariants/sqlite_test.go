package invariants

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoSQLiteModule(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(data)), "sqlite") {
		t.Fatal("go.mod requires a SQLite module; PostgreSQL through the Hub is the only application database")
	}
}
