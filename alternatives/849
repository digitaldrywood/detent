package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationRemovesRetiredKeys(t *testing.T) {
	t.Parallel()
	fixture, err := os.ReadFile("testdata/local-era.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, frontmatter := range []bool{false, true} {
		t.Run(map[bool]string{false: "split", true: "frontmatter"}[frontmatter], func(t *testing.T) {
			dir := t.TempDir()
			workflow := filepath.Join(dir, "WORKFLOW.md")
			raw := fixture
			target := filepath.Join(dir, "detent.yaml")
			if frontmatter {
				target = workflow
				raw = append(append([]byte("---\n"), fixture...), []byte("---\nKeep instructions.\n")...)
			} else if err := os.WriteFile(workflow, []byte("Keep instructions.\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadWorkflow(workflow); err == nil || !strings.Contains(err.Error(), "retired configuration key") {
				t.Fatalf("load before migration = %v", err)
			}
			changed, err := MigrateTrackerLanesFile(target)
			if err != nil || !changed {
				t.Fatalf("migration = %t, %v", changed, err)
			}
			loaded, err := LoadWorkflow(workflow)
			if err != nil {
				t.Fatalf("load migrated fixture: %v", err)
			}
			if !strings.Contains(loaded.Prompt, "Keep instructions.") {
				t.Fatalf("lost prompt: %s", loaded.Prompt)
			}
			migrated, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(migrated, []byte("active_states:")) || !bytes.Contains(migrated, []byte("lanes:")) {
				t.Fatalf("lanes not migrated: %s", migrated)
			}
			changed, err = MigrateTrackerLanesFile(target)
			if err != nil || changed {
				t.Fatalf("second migration = %t, %v", changed, err)
			}
		})
	}
}
