package cloudentry

import (
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestSQLiteSchemaInventory(t *testing.T) {
	t.Parallel()
	for _, fixture := range []struct {
		name          string
		applicationID int64
		seed          string
	}{
		{"registry", registryApplicationID, `
INSERT INTO organizations(id,name,state,endpoint,generation,created_at,updated_at)
VALUES ('org_example','Example organization','ready','https://example.invalid',1,'2026-01-02T03:04:05Z','2026-01-02T03:04:05.123Z'),
('org_other','Other organization','requested','',2,'2026-01-02T03:04:05Z','2026-01-02T03:04:05Z');
INSERT INTO organization_events(organization_id,event,generation,recorded_at)
VALUES ('org_example','ready',1,'2026-01-02T03:04:05Z'),('org_other','requested',2,'2026-01-02T03:04:05.123456789Z');
`},
		{"auth", authApplicationID, `
INSERT INTO sessions(token_hash,subject,email,csrf_secret,identity_json,created_at,expires_at,revoked_at)
VALUES ('example_session','example_subject','example@example.invalid','example_csrf','{}','2026-01-02T03:04:05Z','2026-01-03T03:04:05Z',NULL),
('other_session','other_subject','other@example.invalid','other_csrf','{}','2026-01-02T03:04:05.123Z','2026-01-03T03:04:05.123Z','2026-01-02T04:04:05Z');
INSERT INTO authorizations(binding,session_hash,organization_id,identity_json,created_at,expires_at)
VALUES ('example_binding','example_session','org_example','{}','2026-01-02T03:04:05Z','2026-01-03T03:04:05Z'),
('other_binding','other_session','org_other','{}','2026-01-02T03:04:05.123Z','2026-01-03T03:04:05.123Z');
INSERT INTO audit(subject,organization_id,event,recorded_at,query_length)
VALUES ('example_subject','org_example','query','2026-01-02T03:04:05Z',NULL),
('other_subject','org_other','query','2026-01-02T03:04:05.123456789Z',12);
`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			s, err := openStore(t.Context(), filepath.Join(t.TempDir(), fixture.name+".db"), fixture.applicationID, fixture.name)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			})
			if _, err := s.db.ExecContext(t.Context(), fixture.seed); err != nil {
				t.Fatal(err)
			}
			testenv.CheckSQLiteInventory(t, s.db.DB, filepath.Join("testdata", fixture.name))
		})
	}
}
