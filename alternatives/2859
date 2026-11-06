package postgres

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestRoleMigrationsRewritesOnlyLegacyRoleIdentifiers(t *testing.T) {
	source := fstest.MapFS{
		"001_roles.sql": {Data: []byte("GRANT detent_runtime TO detent_platform;\nSELECT 'detent_migration';\n-- detent_runtime\nCREATE ROLE detent_platform_extra;\nCREATE ROLE detent_runtime$shadow;\nCREATE ROLE detent_runtime_v2;\nCREATE ROLE café_detent_runtime;\n")},
		"notes.txt":     {Data: []byte("detent_runtime")},
	}
	migrations := RoleMigrations(source)
	got, err := fs.ReadFile(migrations, "001_roles.sql")
	if err != nil {
		t.Fatal(err)
	}
	want := "GRANT detent_runtime_v2 TO detent_platform_v2;\nSELECT 'detent_migration_v2';\n-- detent_runtime_v2\nCREATE ROLE detent_platform_extra;\nCREATE ROLE detent_runtime$shadow;\nCREATE ROLE detent_runtime_v2;\nCREATE ROLE café_detent_runtime;\n"
	if string(got) != want {
		t.Fatalf("rewritten migration = %q, want %q", got, want)
	}
	unchanged, err := fs.ReadFile(migrations, "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(unchanged) != "detent_runtime" {
		t.Fatalf("non-SQL file changed: %q", unchanged)
	}
	original, err := fs.ReadFile(source, "001_roles.sql")
	if err != nil {
		t.Fatal(err)
	}
	wantOriginal := "GRANT detent_runtime TO detent_platform;\nSELECT 'detent_migration';\n-- detent_runtime\nCREATE ROLE detent_platform_extra;\nCREATE ROLE detent_runtime$shadow;\nCREATE ROLE detent_runtime_v2;\nCREATE ROLE café_detent_runtime;\n"
	if string(original) != wantOriginal {
		t.Fatalf("source migration = %q, want unchanged %q", original, wantOriginal)
	}
	if repeated := rewriteMigrationRoles(got); string(repeated) != want {
		t.Fatalf("rewritten migration changed on second pass: %q", repeated)
	}
}
