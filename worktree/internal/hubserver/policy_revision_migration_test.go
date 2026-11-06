package hubserver

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/policy"
)

func TestPolicyRevisionInputsMigrationPreservesHistory(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "hub.db"), defaultBusyTimeout))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	data, err := migrationFiles.ReadFile("migrations/00010_project_policies.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, _, _ := strings.Cut(string(data), "-- +goose Down")
	if _, err := db.ExecContext(t.Context(), up); err != nil {
		t.Fatal(err)
	}
	type revision struct {
		scope      string
		descriptor policy.Descriptor
		raw        string
	}
	var revisions []revision
	for _, scope := range []string{"repository:example-org/first", "repository:example-org/second"} {
		for _, commit := range []string{"", strings.Repeat("a", 40), strings.Repeat("b", 40)} {
			descriptor := hubTestPolicy()
			if commit != "" {
				descriptor.Workflow = &policy.Workflow{Source: "detent.yaml", Revision: commit, States: nativeFixtureStates()}
			}
			descriptor = descriptor.WithID()
			raw, err := json.Marshal(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), "INSERT INTO policy_revisions(scope, policy_id, metadata_json, approved_by, approved_at) VALUES(?,?,?,?,?)", scope, descriptor.ID, string(raw), "administrator", testTimestamp); err != nil {
				t.Fatal(err)
			}
			revisions = append(revisions, revision{scope, descriptor, string(raw)})
		}
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := migratePolicyRevisionInputs(t.Context(), tx); err != nil {
		t.Fatal(err, tx.Rollback())
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, revision := range revisions {
		var raw, actor, at, digest, commit string
		if err := db.QueryRowContext(t.Context(), "SELECT metadata_json, approved_by, approved_at, source_digest, workflow_revision FROM policy_revisions WHERE scope=? AND policy_id=?", revision.scope, revision.descriptor.ID).Scan(&raw, &actor, &at, &digest, &commit); err != nil {
			t.Fatal(err)
		}
		wantCommit := ""
		if revision.descriptor.Workflow != nil {
			wantCommit = revision.descriptor.Workflow.Revision
		}
		if raw != revision.raw || actor != "administrator" || at != testTimestamp || digest != revision.descriptor.SourceDigest || commit != wantCommit {
			t.Fatalf("migration changed audit record or lost typed inputs: %s %s %s %s %s", raw, actor, at, digest, commit)
		}
		var resolved policy.Descriptor
		if err := db.QueryRowContext(t.Context(), "SELECT metadata_json FROM policy_revisions WHERE scope=? AND source_digest=? AND workflow_revision=? ORDER BY approved_at DESC, policy_id LIMIT 1", revision.scope, digest, wantCommit).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), &resolved); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(resolved, revision.descriptor) {
			t.Fatalf("typed history lookup selected wrong revision: %+v", resolved)
		}
	}
}
