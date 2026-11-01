package cloudentry

import "testing"

func TestLatestSchemaVersionsIncludeEachSQLiteStore(t *testing.T) {
	versions, err := LatestSchemaVersions()
	if err != nil {
		t.Fatal(err)
	}
	if versions.Registry != int64(latestMigrationVersion(t, "migrations/registry")) || versions.Auth != int64(latestMigrationVersion(t, "migrations/auth")) {
		t.Fatalf("latest schema versions = %+v", versions)
	}
}
