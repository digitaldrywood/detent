package hubserver

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestSQLiteSchemaInventory(t *testing.T) {
	t.Parallel()
	for _, fixture := range []struct {
		name string
		open func(*testing.T) *sql.DB
	}{
		{"hub", sqliteHubInventoryFixture},
		{"metrics", sqliteMetricsInventoryFixture},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			db := fixture.open(t)
			testenv.CheckSQLiteInventory(t, db, filepath.Join("testdata", fixture.name))
		})
	}
}

func sqliteHubInventoryFixture(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.db")
	seedHubDatabaseImage(t, path, true)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := db.ExecContext(t.Context(), `
PRAGMA foreign_keys=ON;
INSERT INTO organizations(id,name,local,created_at) VALUES ('org_example','Example organization',0,'2026-01-02T03:04:05.123456789Z');
INSERT INTO projects(id,organization_id,name,profile,states_json,require_dependencies,created_at,github_repository_enabled)
VALUES ('prj_example','org_example','Example project','native','[]',0,'2026-01-02T03:04:05Z',0);
INSERT INTO issues(native_id,organization_id,project_id,number,title,body,url,github_state,source_version,source_updated_at,synchronized_at,created_at,updated_at)
SELECT 'wi_inventory_' || id,organization_id,id,1,'Example issue','Example issue body','','open','1',
'2026-01-02T03:04:05Z','2026-01-02T03:04:05Z','2026-01-02T03:04:05Z','2026-01-02T03:04:05Z' FROM projects;
INSERT INTO native_comments(id,organization_id,project_id,work_item_id,revision,sequence,body,actor_json,created_at,updated_at)
SELECT 'comment_' || native_id,organization_id,project_id,native_id,1,1,'Example comment','{"kind":"human","principal_id":"example"}',
'2026-01-02T03:04:05Z','2026-01-02T03:04:05.123456789Z' FROM issues;
INSERT INTO project_secrets(organization_id,project_id,kind,organization_slug,ciphertext,nonce,wrapped_data_key,master_key_version,updated_at)
SELECT organization_id,id,'example','example',x'0102',x'0304',x'0506',1,'2026-01-02T03:04:05Z' FROM projects;
`); err != nil {
		t.Fatal(err)
	}
	return db
}

func sqliteMetricsInventoryFixture(t *testing.T) *sql.DB {
	t.Helper()
	m := testRequestMetrics(t, filepath.Join(t.TempDir(), "metrics.db"), "org_example")
	if _, err := m.db.ExecContext(t.Context(), `
INSERT INTO request_minutes VALUES (1,'GET /items','runner','runner_example',2,2,1.5,0.5,1.0,1.0,'[1,1]');
INSERT INTO request_hours VALUES (0,'GET /items',2,2,1.5,0.5,1.0,1.0,'[1,1]');
INSERT INTO request_days VALUES (0,'GET /items','runner',2,2,1.5,1.0);
`); err != nil {
		t.Fatal(err)
	}
	return m.db
}
