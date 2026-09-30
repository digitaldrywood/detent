package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var hubDatabaseTemplate struct {
	once sync.Once
	dir  string
	path string
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if hubDatabaseTemplate.dir != "" {
		_ = os.RemoveAll(hubDatabaseTemplate.dir)
	}
	os.Exit(code)
}

func seedHubDatabaseTemplate(t *testing.T, path string) {
	t.Helper()
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	if _, err := os.Stat(path); err == nil {
		return
	}
	hubDatabaseTemplate.once.Do(buildHubDatabaseTemplate)
	if hubDatabaseTemplate.err != nil {
		t.Fatalf("build hub database template: %v", hubDatabaseTemplate.err)
	}
	data, err := os.ReadFile(hubDatabaseTemplate.path)
	if err != nil {
		t.Fatalf("read hub database template: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("seed hub database: %v", err)
	}
}

func buildHubDatabaseTemplate() {
	dir, err := os.MkdirTemp("", "hub-database-template-")
	if err != nil {
		hubDatabaseTemplate.err = err
		return
	}
	hubDatabaseTemplate.dir = dir
	path := filepath.Join(dir, "hub.db")
	db, err := sql.Open("sqlite", sqliteDSN(path, defaultBusyTimeout))
	if err != nil {
		hubDatabaseTemplate.err = fmt.Errorf("open template: %w", err)
		return
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx := context.Background()
	store := &database{db: db, path: path}
	err = errors.Join(
		store.configure(ctx, defaultBusyTimeout),
		store.verifyIdentity(ctx),
		store.enableWAL(ctx),
	)
	if err == nil {
		_, err = runMigrations(ctx, db, discardLogger())
	}
	hubDatabaseTemplate.err = errors.Join(err, db.Close())
	if hubDatabaseTemplate.err == nil {
		hubDatabaseTemplate.path = path
	}
}
