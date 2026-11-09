package postgres

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/schemafingerprint"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

func LatestSchemaVersion() (int64, error) {
	versions, err := schemaVersions()
	if err != nil {
		return 0, err
	}
	var latest int64
	for _, version := range versions {
		latest = max(latest, version)
	}
	return latest, nil
}

func LatestSchemaFingerprint() (string, error) {
	versions, err := schemaVersions()
	if err != nil {
		return "", err
	}
	return schemafingerprint.Versions(versions), nil
}

func schemaVersions() ([]int64, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	versions := make([]int64, 0, len(entries))
	for _, entry := range entries {
		prefix, _, _ := strings.Cut(entry.Name(), "_")
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse PostgreSQL migration version: %w", err)
		}
		versions = append(versions, version)
	}
	source, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	for _, migration := range ConversationMigrations(source) {
		versions = append(versions, migration.Version)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	return versions, nil
}
