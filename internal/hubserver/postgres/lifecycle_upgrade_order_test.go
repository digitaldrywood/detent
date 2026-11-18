package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const upgradeOrderHead int64 = 20261109184650

var upgradeOrderPending = []int64{
	20261108175144,
	20261108215450,
	20261108224926,
	conversationContinuationBackfillVersion,
	20261108232743,
	20261108233605,
	20261109012539,
	20261109030624,
	20261109030812,
	20261109033258,
	20261109033415,
	20261109060057,
	20261109073347,
	20261109092232,
	20261109092607,
	20261109100704,
	20261109123421,
	20261109163830,
	20261109164729,
	20261109182156,
	20261109192211,
	20261109195442,
	20261109212021,
	20261109222640,
	20261109224251,
	20261110013206,
	20261110031053,
	20261110042737,
	20261110042748,
	20261110042758,
	20261110154406,
	20261110164026,
	20261110215657,
	20261110221710,
	20261110233338,
	20261111005441,
	20261111010534,
	20261111011502,
	20261111015700,
	20261111015701,
	20261111022029,
	20261111022030,
	20261111030542,
	20261111031208,
	20261111045519,
	20261111045520,
	20261111045521,
	20261111045522,
	20261111055218,
	20261111055219,
	20261111062810,
	20261111062811,
	20261111062812,
	20261111062813,
	20261111062814,
	20261111064145,
	20261111131717,
	20261111133122,
	20261111150608,
	20261111151226,
	20261111151640,
	20261111180030,
	20261111192957,
	20261111192959,
	20261111193000,
	20261111193001,
	20261111194045,
	20261111194120,
	20261111194206,
	20261111195645,
	20261111195654,
	20261111195659,
	20261111200015,
	20261111200105,
	20261111201829,
	20261111201834,
	20261111202405,
	20261111202406,
	20261111203018,
	20261111203023,
	20261111210238,
	20261111211428,
	20261111211430,
	20261111211431,
	20261111213835,
	20261111215906,
	20261111221622,
	20261111221627,
	20261111221632,
	20261111222854,
	20261111225811,
	20261111230546,
	20261111231501,
	20261111232904,
	20261111232911,
	20261111234250,
	20261111234251,
	20261111234252,
	20261111234253,
	20261111234254,
	20261111235348,
	20261111235349,
	20261111235350,
	20261111235351,
	20261112001212,
	20261112001213,
	20261112001214,
	20261112001215,
	20261112004725,
	20261112013604,
	20261112013627,
	20261112031630,
	20261112032820,
	20261112032821,
	20261112032822,
	20261112033754,
	20261112035939,
	20261112042052,
	20261112042057,
	20261112042102,
	20261112042107,
	20261112043935,
	20261112043936,
	20261112043937,
	20261112043938,
	20261112043939,
	20261112043940,
	20261112043941,
	20261112043943,
	20261112043944,
	20261112082128,
	20261112092845,
	20261112101746,
	20261112105114,
	20261112112201,
	20261112124958,
	20261112130601,
	20261112135609,
	20261112155646,
	20261112175135,
	20261113001627,
	20261113001912,
	20261113004758,
	20261113145207,
	20261113152627,
	20261113155409,
	20261113165539,
	20261113170824,
	20261113174302,
	20261113175607,
	20261113205502,
	20261113223115,
	20261114011610,
	20261114014509,
	20261114014702,
	20261114041647,
	20261114052502,
	20261114055257,
	20261114073630,
	20261114131438,
	20261114132741,
	20261114134204,
	20261114134551,
	20261114141245,
	20261114185520,
	20261116042557,
	20261116063135,
	20261116203041,
	20261116222600,
	20261116222700,
	20261116222800,
	20261116234027,
	20261117043721, 20261118021624, 20261118085855, 20261118091938, 20261118121352,
}

var upgradeOrderTenants = []struct {
	organization tracker.OrganizationID
	weight       int64
	continuation bool
}{
	{organization: "org_first", weight: 1, continuation: true},
	{organization: "org_second", weight: 2, continuation: false},
}

func TestMigrateAppliesPendingVersionsBelowPopulatedHead(t *testing.T) {
	t.Parallel()
	const concurrentIndexes = "20261109033415_doctor_proposal_identity_indexes.sql"
	source := os.DirFS("migrations")
	files, err := fs.Glob(source, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	head := populatedHeadMigrations(t)
	if registered := ConversationMigrations(head); len(registered) != 0 {
		t.Fatalf("populated head registered %d conversation migrations", len(registered))
	}
	if registered := ConversationMigrations(source); len(registered) != 1 || registered[0].Version != conversationContinuationBackfillVersion {
		t.Fatalf("full source conversation migrations = %+v", registered)
	}
	populated := PopulatedHeadTemplate(t, seedUpgradeOrderTenants)
	want := catalogSnapshot(t, migratedTemplate(t, source, nil).Database(t))

	for _, interrupted := range []bool{false, true} {
		name := "upgrade"
		if interrupted {
			name = "resume interrupted concurrent index"
		}
		t.Run(name, func(t *testing.T) {
			db := populated.Database(t)
			config := migrationTestLogin(t, db, MigrationRoleName)
			var count int
			var version int64
			if err := db.QueryRowContext(t.Context(), "SELECT count(*), max(version_id) FROM goose_db_version WHERE version_id > 0 AND is_applied").Scan(&count, &version); err != nil {
				t.Fatal(err)
			}
			if count != 223 || version != upgradeOrderHead {
				t.Fatalf("populated head ledger = %d/%d", count, version)
			}
			var retainedColumn bool
			if err := db.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='change_issue_links'::regclass AND attname='retained_pr_version_id' AND NOT attisdropped)`).Scan(&retainedColumn); err != nil {
				t.Fatal(err)
			}
			var assignments int
			if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM runner_lease_assignments").Scan(&assignments); err != nil || assignments != 6 {
				t.Fatalf("historical runner assignments = %d, want 6: %v", assignments, err)
			}
			if retainedColumn {
				t.Fatal("populated head already has retained PR metadata")
			}
			if err := validateSchema(t.Context(), db, source); !errors.Is(err, ErrSchema) {
				t.Fatalf("incomplete schema validation = %v", err)
			}
			if slices.Equal(catalogSnapshot(t, db), want) {
				t.Fatal("populated head catalog already matches a fresh install")
			}
			if interrupted {
				partial := fstest.MapFS{}
				for _, file := range files {
					data, err := fs.ReadFile(source, file)
					if err != nil {
						t.Fatal(err)
					}
					if file == concurrentIndexes {
						const second = "DROP INDEX CONCURRENTLY IF EXISTS doctor_proposal_identity_state;"
						replaced := strings.Replace(string(data), second, "SELECT 1/0;\n"+second, 1)
						if replaced == string(data) {
							t.Fatal("concurrent index migration has no second build to interrupt")
						}
						data = []byte(replaced)
					}
					partial[file] = &fstest.MapFile{Data: data}
				}
				err := Migrate(t.Context(), config, partial)
				var pgerr *pgconn.PgError
				if !errors.As(err, &pgerr) || pgerr.Code != "22012" {
					t.Fatalf("partial migration error = %v, want division by zero", err)
				}
				if err := validateSchema(t.Context(), db, source); !errors.Is(err, ErrSchema) {
					t.Fatalf("partial schema validation = %v", err)
				}
				var applied int64
				if err := db.QueryRowContext(t.Context(), "SELECT version_id FROM goose_db_version WHERE is_applied ORDER BY id DESC LIMIT 1").Scan(&applied); err != nil {
					t.Fatal(err)
				}
				if applied != 20261109033258 {
					t.Fatalf("last version applied before interruption = %d", applied)
				}
				var built, remaining bool
				if err := db.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_index WHERE indexrelid=to_regclass('doctor_proposal_unknown_heads') AND indisvalid), to_regclass('doctor_proposal_identity_state') IS NOT NULL`).Scan(&built, &remaining); err != nil {
					t.Fatal(err)
				}
				if !built || remaining {
					t.Fatalf("interrupted index builds: first=%t second=%t", built, remaining)
				}
				if _, err := db.ExecContext(t.Context(), `UPDATE pg_index SET indisvalid=false WHERE indexrelid='doctor_proposal_unknown_heads'::regclass`); err != nil {
					t.Fatal(err)
				}
			}
			if interrupted {
				partial := fstest.MapFS{}
				for _, file := range files {
					data, err := fs.ReadFile(source, file)
					if err != nil {
						t.Fatal(err)
					}
					if file == "20261111133122_repair_lease_runner_project_initialization.sql" {
						const committed = "after_expiry := next_expiry;\n   after_lease := next_lease;\n   COMMIT;"
						replaced := strings.Replace(string(data), committed, committed+"\n   PERFORM 1/0;", 1)
						if replaced == string(data) {
							t.Fatal("lease repair has no project batch commit to interrupt")
						}
						data = []byte(replaced)
					}
					partial[file] = &fstest.MapFile{Data: data}
				}
				err := Migrate(t.Context(), config, partial)
				var pgerr *pgconn.PgError
				if !errors.As(err, &pgerr) || pgerr.Code != "22012" {
					t.Fatalf("partial lease repair error = %v, want division by zero", err)
				}
				var projects, runners int
				if err := db.QueryRowContext(t.Context(), "SELECT count(*) FILTER (WHERE project_id IS NOT NULL), count(*) FILTER (WHERE runner_id IS NOT NULL) FROM leases").Scan(&projects, &runners); err != nil {
					t.Fatal(err)
				}
				if projects != 2 || runners != 0 {
					t.Fatalf("partial lease repair projects/runners = %d/%d, want 2/0", projects, runners)
				}
			}
			if interrupted {
				partial := fstest.MapFS{}
				for _, file := range files {
					data, err := fs.ReadFile(source, file)
					if err != nil {
						t.Fatal(err)
					}
					if file == "20261111192959_attempt_project_initialize.sql" {
						const committed = "after_attempt := next_attempt;\n   COMMIT;"
						changed := strings.Replace(string(data), committed, committed+"\n   PERFORM 1/0;", 1)
						if changed == string(data) {
							t.Fatal("attempt initialization has no batch commit to interrupt")
						}
						data = []byte(changed)
					}
					partial[file] = &fstest.MapFile{Data: data}
				}
				err := Migrate(t.Context(), config, partial)
				var failure *pgconn.PgError
				if !errors.As(err, &failure) || failure.Code != "22012" {
					t.Fatalf("partial attempt initialization error=%v", err)
				}
				var initialized int
				if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_attempts WHERE project_id IS NOT NULL").Scan(&initialized); err != nil || initialized != 1 {
					t.Fatalf("committed attempt initialization=%d error=%v", initialized, err)
				}
				result, err := db.ExecContext(t.Context(), `UPDATE native_attempts SET updated_at=updated_at+interval '1 microsecond' WHERE project_id IS NULL`)
				if err != nil {
					t.Fatalf("uninitialized attempt update failed during committed migration batches: %v", err)
				}
				if rows, err := result.RowsAffected(); err != nil || rows != 1 {
					t.Fatalf("uninitialized attempt rows=%d error=%v", rows, err)
				}
			}
			if interrupted {
				partial := fstest.MapFS{}
				for _, file := range files {
					data, err := fs.ReadFile(source, file)
					if err != nil {
						t.Fatal(err)
					}
					if file == "20261111211430_version_landing_initialize.sql" {
						const committed = "after_version := next_version;\n   COMMIT;"
						changed := strings.Replace(string(data), committed, committed+"\n   PERFORM 1/0;", 1)
						if changed == string(data) {
							t.Fatal("version initialization has no batch commit to interrupt")
						}
						data = []byte(changed)
					}
					partial[file] = &fstest.MapFile{Data: data}
				}
				err := Migrate(t.Context(), config, partial)
				var failure *pgconn.PgError
				if !errors.As(err, &failure) || failure.Code != "22012" {
					t.Fatalf("partial version initialization error=%v", err)
				}
				var initialized int
				if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM change_versions WHERE project_id IS NOT NULL").Scan(&initialized); err != nil || initialized != 4 {
					t.Fatalf("committed version initialization=%d error=%v", initialized, err)
				}
				_, err = db.ExecContext(t.Context(), "UPDATE change_versions SET external_id=external_id WHERE project_id IS NULL")
				var immutable *pgconn.PgError
				if !errors.As(err, &immutable) || immutable.Code != "23514" || immutable.Message != "history is immutable" {
					t.Fatalf("uninitialized immutable version update=%v", err)
				}
				var remaining int
				if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM change_versions WHERE project_id IS NULL").Scan(&remaining); err != nil || remaining != 4 {
					t.Fatalf("uninitialized version rows=%d error=%v", remaining, err)
				}
			}
			if interrupted {
				partial := fstest.MapFS{}
				for _, file := range files {
					data, err := fs.ReadFile(source, file)
					if err != nil {
						t.Fatal(err)
					}
					if file == "20261111234251_evidence_project_initialize.sql" {
						const committed = "after_change := next_change; after_sequence := next_sequence;\n   COMMIT;"
						changed := strings.Replace(string(data), committed, committed+"\n   PERFORM 1/0;", 1)
						if changed == string(data) {
							t.Fatal("evidence initialization has no batch commit to interrupt")
						}
						data = []byte(changed)
					}
					partial[file] = &fstest.MapFile{Data: data}
				}
				err := Migrate(t.Context(), config, partial)
				var failure *pgconn.PgError
				if !errors.As(err, &failure) || failure.Code != "22012" {
					t.Fatalf("partial evidence initialization error=%v", err)
				}
				var initialized int
				if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM change_evidence WHERE project_id IS NOT NULL").Scan(&initialized); err != nil || initialized != 2 {
					t.Fatalf("committed evidence initialization=%d error=%v", initialized, err)
				}
				_, err = db.ExecContext(t.Context(), "UPDATE change_evidence SET actor_principal_id=actor_principal_id WHERE project_id IS NULL")
				var immutable *pgconn.PgError
				if !errors.As(err, &immutable) || immutable.Code != "23514" || immutable.Message != "history is immutable" {
					t.Fatalf("uninitialized immutable evidence update=%v", err)
				}
			}
			if interrupted {
				partial := fstest.MapFS{}
				for _, file := range files {
					data, err := fs.ReadFile(source, file)
					if err != nil {
						t.Fatal(err)
					}
					if file == "20261112001213_evidence_quality_initialize.sql" {
						const committed = "after_change := next_change; after_sequence := next_sequence;\n   COMMIT;"
						changed := strings.Replace(string(data), committed, committed+"\n   PERFORM 1/0;", 1)
						if changed == string(data) {
							t.Fatal("quality initialization has no committed batch to interrupt")
						}
						data = []byte(changed)
					}
					partial[file] = &fstest.MapFile{Data: data}
				}
				err := Migrate(t.Context(), config, partial)
				var failure *pgconn.PgError
				if !errors.As(err, &failure) || failure.Code != "22012" {
					t.Fatalf("partial quality initialization error=%v", err)
				}
				var initialized int
				if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM change_evidence WHERE quality_at IS NOT NULL").Scan(&initialized); err != nil || initialized != 2 {
					t.Fatalf("committed quality initialization=%d error=%v", initialized, err)
				}
				_, err = db.ExecContext(t.Context(), "UPDATE change_evidence SET quality_at=quality_at WHERE quality_at IS NULL")
				var immutable *pgconn.PgError
				if !errors.As(err, &immutable) || immutable.Code != "23514" || immutable.Message != "history is immutable" {
					t.Fatalf("uninitialized quality evidence mutated=%v", err)
				}
			}
			if err := Migrate(t.Context(), config, source); err != nil {
				t.Fatal(err)
			}
			var reviewTimes, escapeTimes int
			if err := db.QueryRowContext(t.Context(), `SELECT count(*) FILTER(WHERE e.kind='review' AND e.quality_at=e.created_at),count(*) FILTER(WHERE e.kind='discussion' AND e.quality_at=f.observed_at AND e.quality_at<e.created_at) FROM change_evidence e LEFT JOIN report_escape_facts f ON f.organization_id=e.organization_id AND f.change_id=e.change_id AND f.evidence_sequence=e.sequence`).Scan(&reviewTimes, &escapeTimes); err != nil || reviewTimes != 2 || escapeTimes != 2 {
				t.Fatalf("retained quality times reviews=%d escapes=%d error=%v", reviewTimes, escapeTimes, err)
			}
			var evidenceProjects, badEvidenceProjects, preservedEvidence int
			if err := db.QueryRowContext(t.Context(), `SELECT count(*),count(*) FILTER(WHERE e.project_id IS DISTINCT FROM c.project_id),count(*) FILTER(WHERE e.payload_sha256=repeat('c',64) AND ((e.sequence=1 AND e.kind='review') OR (e.sequence=2 AND e.kind='discussion')) AND e.actor_principal_id='actor_example') FROM change_evidence e LEFT JOIN change_requests c ON c.organization_id=e.organization_id AND c.id=e.change_id`).Scan(&evidenceProjects, &badEvidenceProjects, &preservedEvidence); err != nil || evidenceProjects != 4 || badEvidenceProjects != 0 || preservedEvidence != 4 {
				t.Fatalf("retained evidence initialization=%d mismatches=%d preserved=%d error=%v", evidenceProjects, badEvidenceProjects, preservedEvidence, err)
			}

			var versionProjects, badVersionProjects, knownVersionHistories int
			if err := db.QueryRowContext(t.Context(), `SELECT count(*),count(*) FILTER(WHERE v.project_id IS DISTINCT FROM c.project_id),count(*) FILTER(WHERE v.landing_history_known) FROM change_versions v LEFT JOIN change_requests c ON c.organization_id=v.organization_id AND c.id=v.change_id`).Scan(&versionProjects, &badVersionProjects, &knownVersionHistories); err != nil || versionProjects != 8 || badVersionProjects != 0 || knownVersionHistories != 0 {
				t.Fatalf("retained version initialization: rows=%d mismatches=%d invented-history=%d error=%v", versionProjects, badVersionProjects, knownVersionHistories, err)
			}
			if _, err := db.ExecContext(t.Context(), `INSERT INTO change_versions(organization_id,id,change_id,number,head_sha,base_sha,merge_base_sha,repository,policy_revision_id,actor_kind,actor_principal_id,external_provider,external_id,external_url,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,project_id) SELECT organization_id,'version_wrong_project',change_id,9999,head_sha,base_sha,merge_base_sha,repository,policy_revision_id,actor_kind,actor_principal_id,external_provider,external_id,external_url,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,'prj_other' FROM change_versions WHERE organization_id='org_first' AND id='version_change_pr_none_1'`); err == nil {
				t.Fatal("version accepted another project's change")
			} else {
				var failure *pgconn.PgError
				if !errors.As(err, &failure) || failure.Code != "23503" || failure.ConstraintName != "change_versions_project_identity" {
					t.Fatalf("version project constraint=%v", err)
				}
			}

			var attemptProjects, badAttemptProjects int
			if err := db.QueryRowContext(t.Context(), `SELECT count(*),count(*) FILTER(WHERE a.project_id IS DISTINCT FROM i.project_id) FROM native_attempts a LEFT JOIN work_item_identities i ON i.organization_id=a.organization_id AND i.id=a.issue_id`).Scan(&attemptProjects, &badAttemptProjects); err != nil || attemptProjects != 2 || badAttemptProjects != 0 {
				t.Fatalf("retained attempt project initialization: rows=%d mismatches=%d error=%v", attemptProjects, badAttemptProjects, err)
			}
			if _, err := db.ExecContext(t.Context(), `UPDATE native_attempts SET project_id='prj_other' WHERE organization_id='org_first' AND id='attempt_shared'`); err == nil {
				t.Fatal("attempt project identity accepted another project")
			} else {
				var failure *pgconn.PgError
				if !errors.As(err, &failure) || failure.Code != "23503" || failure.ConstraintName != "native_attempts_project_identity" {
					t.Fatalf("attempt identity failure=%v", err)
				}
			}
			if err := validateSchema(t.Context(), db, source); err != nil {
				t.Fatal(err)
			}
			rows, err := db.QueryContext(t.Context(), "SELECT version_id FROM goose_db_version WHERE is_applied AND id>(SELECT max(id) FROM goose_db_version WHERE version_id=$1) ORDER BY id", upgradeOrderHead)
			if err != nil {
				t.Fatal(err)
			}
			var applied []int64
			for rows.Next() {
				var version int64
				if err := rows.Scan(&version); err != nil {
					t.Fatal(err)
				}
				applied = append(applied, version)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(applied, upgradeOrderPending) {
				t.Fatalf("pending application order = %v, want %v", applied, upgradeOrderPending)
			}
			var before, after int
			if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM goose_db_version").Scan(&before); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(t.Context(), config, source); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM goose_db_version").Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("idempotent migration changed ledger: %d -> %d", before, after)
			}
			if got := catalogSnapshot(t, db); !slices.Equal(got, want) {
				t.Errorf("upgraded catalog differs from a fresh install:\n%s", catalogDifference(want, got))
			}
			for _, name := range []string{"collaboration_events_visible_history", "project_artifact_retained_uploads", "doctor_proposal_unknown_heads", "doctor_proposal_identity_state", "native_command_operator_project_active", "native_attempts_local_identity_idx", "native_attempts_local_identity_unknown_idx"} {
				var valid bool
				if err := db.QueryRowContext(t.Context(), `SELECT indisvalid AND indisready FROM pg_index WHERE indexrelid=$1::regclass`, name).Scan(&valid); err != nil || !valid {
					t.Errorf("index %s valid=%t error=%v", name, valid, err)
				}
			}
			for _, tenant := range upgradeOrderTenants {
				t.Run(string(tenant.organization)+" backfills", func(t *testing.T) {
					requireUpgradeOrderBackfills(t, db, tenant.organization, tenant.weight, tenant.continuation)
				})
			}
			runtime := migrationTestLogin(t, db, RuntimeRoleName)
			repository, err := Open(t.Context(), Config{Connection: runtime, Pool: PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1}}, source)
			if err != nil {
				t.Fatalf("open upgraded schema: %v", err)
			}
			t.Cleanup(func() {
				if err := repository.Close(); err != nil {
					t.Error(err)
				}
			})
			for _, tenant := range upgradeOrderTenants {
				t.Run(string(tenant.organization)+" typed write", func(t *testing.T) {
					requireUpgradeOrderTypedWrite(t, db, repository, tenant.organization, tenant.weight)
				})
			}
		})
	}
	t.Run("retained attempt initializes after live issue deletion", func(t *testing.T) {
		db := populated.Database(t)
		config := migrationTestLogin(t, db, MigrationRoleName)
		if _, err := db.ExecContext(t.Context(), `UPDATE leases SET released_at=greatest(renewed_at,clock_timestamp()) WHERE issue_id=1`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `DELETE FROM issues WHERE id=1`); err != nil {
			t.Fatal(err)
		}
		landedAt := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
		if _, err := db.ExecContext(t.Context(), `UPDATE change_requests SET landed_version_id=current_version_id,landed_at=$1,landed_head_sha=repeat('1',40),landed_merge_sha=repeat('3',40),landed_base_ref='main' WHERE id='change_pr_referenced'`, landedAt); err != nil {
			t.Fatal(err)
		}
		if err := Migrate(t.Context(), config, source); err != nil {
			t.Fatal(err)
		}
		for _, org := range []string{"org_first", "org_second"} {
			var versionProject, sourceKey, landedKey string
			var known bool
			var at time.Time
			if err := db.QueryRowContext(t.Context(), `SELECT v.project_id,v.landing_history_known,v.landed_at,c.payload_object_key,v.landed_object_key FROM change_versions v JOIN change_requests c ON c.organization_id=v.organization_id AND c.id=v.change_id WHERE v.organization_id=$1 AND v.id='version_change_pr_referenced_4'`, org).Scan(&versionProject, &known, &at, &sourceKey, &landedKey); err != nil || versionProject != "prj_shared" || !known || !at.Equal(landedAt) || landedKey != sourceKey || landedKey == "" {
				t.Fatalf("retained current landing project=%q known=%v at=%v source=%q frozen=%q error=%v", versionProject, known, at, sourceKey, landedKey, err)
			}

			var project, payload string
			var live bool
			if err := db.QueryRowContext(t.Context(), `SELECT a.project_id,a.payload_object_key,EXISTS(SELECT 1 FROM issues i WHERE i.organization_id=a.organization_id AND i.id=a.issue_id) FROM native_attempts a WHERE a.organization_id=$1 AND a.id='attempt_shared'`, org).Scan(&project, &payload, &live); err != nil || project != "prj_shared" || payload != org+"/retained_payload" || live {
				t.Fatalf("retained project=%q payload=%q live=%v error=%v", project, payload, live, err)
			}
		}
	})

}

func populatedHeadMigrations(t *testing.T) fstest.MapFS {
	t.Helper()
	source := os.DirFS("migrations")
	files, err := fs.Glob(source, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	head := fstest.MapFS{}
	for _, name := range files {
		version, err := strconv.ParseInt(name[:14], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(upgradeOrderPending, version) || version > upgradeOrderHead {
			continue
		}
		data, err := fs.ReadFile(source, name)
		if err != nil {
			t.Fatal(err)
		}
		head[name] = &fstest.MapFile{Data: data}
	}
	if len(head) != 223 {
		t.Fatalf("populated head migrations = %d, want 223", len(head))
	}
	return head
}

func PopulatedHeadVersions(t *testing.T) []int64 {
	t.Helper()
	var versions []int64
	for name := range populatedHeadMigrations(t) {
		version, err := strconv.ParseInt(name[:14], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	slices.Sort(versions)
	return versions
}

func PopulatedHeadPendingVersions() []int64 {
	return slices.Clone(upgradeOrderPending)
}

func PopulatedHeadTemplate(t *testing.T, seed func(context.Context, *sql.DB) error) *pgtest.Template {
	t.Helper()
	return migratedTemplate(t, populatedHeadMigrations(t), seed)
}

func seedUpgradeOrderTenants(ctx context.Context, db *sql.DB) error {
	for _, tenant := range upgradeOrderTenants {
		for _, statement := range []string{
			`INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,CURRENT_TIMESTAMP)`,
			`INSERT INTO projects(organization_id,id,name,slug,profile) SELECT $1,'prj_'||suffix,suffix,suffix,'native' FROM unnest(ARRAY['shared','other']) suffix`,
			`INSERT INTO project_workflow_states(organization_id,project_id,name,terminal,dispatchable,board_rank) SELECT $1,p.id,s.name,s.terminal,NOT s.terminal,s.rank FROM projects p CROSS JOIN (VALUES('Todo',false,0),('Done',true,1)) s(name,terminal,rank) WHERE p.organization_id=$1`,
			`INSERT INTO issues(organization_id,id,native_id,project_id,number,title,native_source_key,workflow_state_name,archived)
			 SELECT $1,n,'wi_todo_'||n,'prj_shared',n,'Example','source_todo_'||n,'Todo',false FROM generate_series(1,$2::bigint+1) n
			 UNION ALL SELECT $1,11,'wi_done','prj_shared',11,'Example','source_done','Done',false
			 UNION ALL SELECT $1,12,'wi_archived','prj_shared',12,'Example','source_archived','Todo',true
			 UNION ALL SELECT $1,13,'wi_unset','prj_shared',13,'Example','source_unset',NULL,false
			 UNION ALL SELECT $1,14,'wi_other','prj_other',1,'Example','source_other','Todo',false`,
			`INSERT INTO collaboration_versions(organization_id,project_id,work_item_id,record_id,revision,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
			 SELECT $1,project_id,native_id,native_id,1,'orgs/'||$1||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1 FROM issues WHERE organization_id=$1`,
			`INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,type,schema_version,actor_kind,actor_principal_id,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,payload_format_version)
			 SELECT $1,'prj_shared','wi_todo_1','decision_todo_'||n,n,'scheduler.decision',1,'human','actor_example',CURRENT_TIMESTAMP,'orgs/'||$1||'/blobs/sha256/bb/'||repeat('b',64),repeat('b',64),1,1,2 FROM generate_series(1,$2::bigint+1) n
			 UNION ALL SELECT $1,'prj_other','wi_other','decision_other_1',1,'scheduler.decision',1,'human','actor_example',CURRENT_TIMESTAMP,'orgs/'||$1||'/blobs/sha256/bb/'||repeat('b',64),repeat('b',64),1,1,2
			 UNION ALL SELECT $1,'prj_shared','wi_done','created_done',1,'issue.created',1,'human','actor_example',CURRENT_TIMESTAMP,'orgs/'||$1||'/blobs/sha256/bb/'||repeat('b',64),repeat('b',64),1,1,2`,
			`INSERT INTO change_requests(organization_id,project_id,work_item_id,id,title,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
			 SELECT $1,'prj_shared',c.item,c.id,'Example '||c.id,'orgs/'||$1||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1
			 FROM (VALUES('change_pr_referenced','wi_todo_1'),('change_pr_none','wi_todo_2')) c(id,item)`,
			`INSERT INTO change_versions(organization_id,id,change_id,number,head_sha,base_sha,merge_base_sha,repository,policy_revision_id,actor_kind,actor_principal_id,external_provider,external_id,external_url,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
			 SELECT $1,'version_'||v.change_id||'_'||v.number,v.change_id,v.number,repeat('1',40),repeat('2',40),repeat('2',40),'https://github.com/example/repo','policy_example','human','actor_example',
			 CASE WHEN v.external_id<>'' THEN 'github' ELSE '' END,v.external_id,CASE WHEN v.external_id<>'' THEN 'https://github.com/example/repo/pull/'||v.external_id ELSE '' END,
			 'orgs/'||$1||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1
			 FROM (VALUES('change_pr_referenced',1,'11'),('change_pr_referenced',3,'33'),('change_pr_referenced',4,''),('change_pr_none',1,'')) v(change_id,number,external_id)`,
			`INSERT INTO change_evidence(organization_id,change_id,sequence,version_id,kind,source_key,actor_kind,actor_principal_id,decision,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,review_kind,review_session_known)
             SELECT $1,'change_pr_none',n,'version_change_pr_none_1',CASE WHEN n=1 THEN 'review' ELSE 'discussion' END,'source_evidence_'||n,'human','actor_example',CASE WHEN n=1 THEN 'changes_requested' ELSE '' END,CURRENT_TIMESTAMP,'orgs/'||$1||'/blobs/sha256/cc/'||repeat('c',64),repeat('c',64),1,1,CASE WHEN n=1 THEN 'human' ELSE '' END,true FROM generate_series(1,2)n`,
			`INSERT INTO report_escape_facts(organization_id,change_id,evidence_sequence,occurrence_id,kind,cause,observed_at)
             SELECT organization_id,change_id,sequence,'quality-occurrence','revert','missing_criterion',created_at-interval '1 hour' FROM change_evidence WHERE organization_id=$1 AND sequence=2`,
			`UPDATE change_requests SET current_version_id=CASE id WHEN 'change_pr_referenced' THEN 'version_change_pr_referenced_4' ELSE 'version_change_pr_none_1' END
			 WHERE organization_id=$1 AND id IN ('change_pr_referenced','change_pr_none')`,
			`INSERT INTO change_issue_links(organization_id,project_id,work_item_id,change_id,created_at)
			 SELECT organization_id,project_id,work_item_id,id,created_at FROM change_requests WHERE organization_id=$1 AND id IN ('change_pr_referenced','change_pr_none')`,
			`INSERT INTO machines(organization_id,id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at) SELECT $1,'machine_shared','example',1,'example',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP`,
			`INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at) SELECT $1,'lease_shared',1,'machine_shared','session_shared',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP+interval '1 hour'`,
			`INSERT INTO api_tokens(organization_id,id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at) VALUES($1,'lease_runner_token','Example lease runner',$1||'_lease_runner',$1||'_lease_runner','worker',true,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
			`INSERT INTO runner_enrollments(organization_id,id,runner_id,machine_id,token_hash,scope,operations,created_at,expires_at,created_by) VALUES($1,'lease_enrollment','runner_shared','machine_shared',md5($1)||md5($1),'organization',ARRAY['claim'],CURRENT_TIMESTAMP,CURRENT_TIMESTAMP+interval '1 day','example_operator')`,
			`INSERT INTO runner_identities(organization_id,runner_id,machine_id,token_id,enrollment_id,scope,operations,capacity_limit,reported_capacity,created_at,expires_at,last_heartbeat_at) VALUES($1,'runner_shared','machine_shared','lease_runner_token','lease_enrollment','organization',ARRAY['claim'],3,3,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP+interval '1 day',CURRENT_TIMESTAMP)`,
			`INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at,released_at) VALUES($1,'lease_other',14,'machine_shared','session_other',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP+interval '1 hour',NULL),($1,'lease_released',11,'machine_shared','session_released',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP+interval '1 hour',CURRENT_TIMESTAMP)`,
			`INSERT INTO runner_lease_assignments(organization_id,lease_id,runner_id,assigned_at) SELECT $1,lease_id,'runner_shared',CURRENT_TIMESTAMP FROM leases WHERE organization_id=$1 AND lease_id IN ('lease_shared','lease_other','lease_released')`,
			`INSERT INTO native_attempt_runs(organization_id,run_id,issue_id) VALUES($1,'run_shared',1)`,
			`INSERT INTO native_attempts(organization_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,checkpoint_object_key,started_at,updated_at)
			 SELECT organization_id,'attempt_shared',issue_id,lease_id,fencing_token,'run_shared',1,'succeeded',work_item_revision,0,$1||'/retained_payload',$1||'/retained_checkpoint',acquired_at,renewed_at FROM leases WHERE organization_id=$1 AND lease_id='lease_shared'`,
			`INSERT INTO native_attempt_events(organization_id,attempt_id,sequence,request_hash) SELECT $1,'attempt_shared',n,repeat('c',64) FROM generate_series(1,$2::bigint) n`,
			`INSERT INTO native_command_states(organization_id,actor_id,operation,command_key,request_hash,created_at,response_expired) SELECT $1,'example-history','example.history',n::text,repeat('a',64),CURRENT_TIMESTAMP,true FROM generate_series(1,$2::bigint+1) n`,
			`INSERT INTO api_tokens(organization_id,id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at) SELECT $1,'continuation_owner','Continuation owner',$1,$1,'operator',true,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP`,
			`INSERT INTO conversations(organization_id,id,project_id,owner_principal_id,origin,visibility,status,event_seq) SELECT $1,c.id,'prj_shared','continuation_owner',c.origin,'shared','active',c.head FROM (VALUES('conversation_continue','worker',$2::bigint),('conversation_idle','worker',0),('conversation_user','user',0)) c(id,origin,head)`,
			`INSERT INTO conversation_commands(organization_id,conversation_id,key,kind,request_hash,receipt_json) SELECT $1,'conversation_continue','continue_original','continue','verified_request','{}'`,
			`INSERT INTO conversation_messages(organization_id,id,conversation_id,seq,role,kind,delivery,attempt_id,command_key) SELECT $1,'message_continue','conversation_continue',1,'user','continue','delivered','original_attempt','continue_original'`,
			`INSERT INTO conversation_events(organization_id,conversation_id,seq,type,body_json)
			 SELECT $1,'conversation_continue',1,'message.accepted','{"id":"message_continue","kind":"continue"}'::jsonb
			 UNION ALL SELECT $1,'conversation_continue',2,'execution.updated','{"status":"completed"}'::jsonb WHERE $2::bigint>1`,
		} {
			args := []any{tenant.organization}
			if strings.Contains(statement, "$2") {
				args = append(args, tenant.weight)
			}
			if _, err := db.ExecContext(ctx, statement, args...); err != nil {
				return fmt.Errorf("seed %s populated head: %w", tenant.organization, err)
			}
		}
	}
	return nil
}

func requireUpgradeOrderBackfills(t *testing.T, db *sql.DB, organization tracker.OrganizationID, weight int64, continuation bool) {
	t.Helper()
	text := func(query string, args ...any) string {
		t.Helper()
		var value sql.NullString
		if err := db.QueryRowContext(t.Context(), query, args...).Scan(&value); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return value.String
	}
	for _, check := range []struct{ name, query, want string }{
		{"active population", `SELECT value||' '||initialized FROM hosted_usage_counters WHERE organization_id=$1 AND name='unarchived_issues'`, fmt.Sprintf("%d true", weight+3)},
		{"history population", `SELECT value||' '||initialized FROM hosted_usage_counters WHERE organization_id=$1 AND name='history_records'`, fmt.Sprintf("%d true", (weight+5)+(weight+3)+weight)},
		{"archive accounting", `SELECT count(*)::text FROM archive_usage_counters WHERE organization_id=$1`, "1"},
		{"retained PR historical pointers", `SELECT string_agg(change_id||'/'||work_item_id||'='||COALESCE(retained_pr_version_id,'NULL'),' ' ORDER BY change_id) FROM change_issue_links WHERE organization_id=$1 AND change_id IN ('change_pr_referenced','change_pr_none')`, "change_pr_none/wi_todo_2=NULL change_pr_referenced/wi_todo_1=version_change_pr_referenced_3"},
		{"lease runner and project bindings", `SELECT string_agg(lease_id||'='||COALESCE(project_id,'NULL')||'/'||COALESCE(runner_id,'NULL'),' ' ORDER BY lease_id) FROM leases WHERE organization_id=$1 AND lease_id IN ('lease_shared','lease_other','lease_released')`, "lease_other=prj_other/runner_shared lease_released=NULL/runner_shared lease_shared=prj_shared/runner_shared"},
		{"retained PR immutable sources", `SELECT count(*)||' '||count(*) FILTER (WHERE external_provider='github' AND external_url<>'') FROM change_versions WHERE organization_id=$1 AND change_id IN ('change_pr_referenced','change_pr_none')`, "4 2"},
		{"board populations", `SELECT string_agg(project_id||'/'||kind||'/'||has_state||'/'||state_name||'='||value,' ' ORDER BY project_id,kind,has_state,state_name) FROM local_board_populations WHERE organization_id=$1`,
			fmt.Sprintf("prj_other/project/false/=1 prj_other/state/true/Todo=1 prj_shared/project/false/=%d prj_shared/state/false/=1 prj_shared/state/true/Done=1 prj_shared/state/true/Todo=%d", weight+2, weight+1)},
		{"scheduler heads", `SELECT string_agg(project_id||'/'||work_item_id||'/'||head_kind||'='||id||'@'||sequence,' ' ORDER BY project_id,work_item_id,head_kind) FROM scheduler_event_heads WHERE organization_id=$1`,
			fmt.Sprintf("prj_other/wi_other/latest=decision_other_1@1 prj_other/wi_other/unprojected=decision_other_1@1 prj_shared/wi_todo_1/latest=decision_todo_%[1]d@%[1]d prj_shared/wi_todo_1/unprojected=decision_todo_%[1]d@%[1]d", weight+1)},
		{"event projections", `SELECT count(*)||' '||count(*) FILTER (WHERE NOT history_hidden AND scheduler_outcome='' AND NOT actor_envelope_known AND actor_via_key='' AND actor_display_name='' AND actor_agent='') FROM collaboration_events WHERE organization_id=$1`, fmt.Sprintf("%[1]d %[1]d", weight+3)},
		{"conversation continuation", `SELECT string_agg(id||'='||execution_continuation,' ' ORDER BY id) FROM conversations WHERE organization_id=$1`, fmt.Sprintf("conversation_continue=%t conversation_idle=false conversation_user=false", continuation)},
		{"command custody", `SELECT count(*)||' '||count(*) FILTER (WHERE reservation_project_id IS NULL AND reservation_correlation_id IS NULL AND reservation_claimed_at IS NULL) FROM native_command_states WHERE organization_id=$1`, fmt.Sprintf("%[1]d %[1]d", weight+1)},
		{"attempt facts", `SELECT count(*)||' '||count(*) FILTER (WHERE claim_disposition IS NULL AND local_attempt_id IS NULL AND NOT local_attempt_id_recorded) FROM native_attempts WHERE organization_id=$1`, "1 1"},
		{"doctor identities", `SELECT count(*)||' '||count(*) FILTER (WHERE doctor_identity_revision IS NULL) FROM issues WHERE organization_id=$1`, fmt.Sprintf("%[1]d %[1]d", weight+5)},
	} {
		if got := text(check.query, organization); got != check.want {
			t.Errorf("%s = %q, want %q", check.name, got, check.want)
		}
	}
	for _, check := range []struct{ name, statement, code string }{
		{"continuation required", `INSERT INTO conversations(organization_id,id,project_id,owner_principal_id,origin,visibility,status,execution_continuation) VALUES($1,'conversation_unknown','prj_shared','continuation_owner','user','shared','active',NULL)`, "23502"},
		{"command custody", `UPDATE native_command_states SET reservation_project_id='prj_shared' WHERE organization_id=$1`, "23514"},
		{"claim disposition", `UPDATE native_attempts SET claim_disposition='unknown' WHERE organization_id=$1`, "23514"},
		{"local attempt identity", `UPDATE native_attempts SET local_attempt_id=1 WHERE organization_id=$1`, "23514"},
	} {
		_, err := db.ExecContext(t.Context(), check.statement, organization)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != check.code {
			t.Errorf("%s constraint error = %v, want SQLSTATE %s", check.name, err, check.code)
		}
	}
}

func requireUpgradeOrderTypedWrite(t *testing.T, db *sql.DB, repository *Repository, organization tracker.OrganizationID, weight int64) {
	t.Helper()
	state, actorKind, actorID := "Todo", "human", "actor_example"
	now := time.Now().UTC().Truncate(time.Microsecond)
	var sourceBefore int64
	if err := db.QueryRowContext(t.Context(), `SELECT duplicate_source_sequence FROM projects WHERE organization_id=$1 AND id='prj_shared'`, organization).Scan(&sourceBefore); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ID: 100, ProjectID: "prj_shared", NativeID: "wi_upgraded", Number: 100, Title: "Example", IssueType: "bug", WorkflowStateName: &state, ActorKind: &actorKind, ActorPrincipalID: &actorID, CreatedAt: now})
		return err
	}); err != nil {
		t.Fatalf("typed write after upgrade: %v", err)
	}
	var read corestore.WorkItem
	if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		read, err = r.WorkItem(t.Context(), "prj_shared", "wi_upgraded")
		return err
	}); err != nil {
		t.Fatalf("typed read after upgrade: %v", err)
	}
	if read.OrganizationID != organization || read.Number != 100 || read.WorkflowStateName == nil || *read.WorkflowStateName != "Todo" || read.DoctorIdentityRevision != nil {
		t.Fatalf("typed read after upgrade = %+v", read)
	}
	var active, todo, sourceAfter int64
	if err := db.QueryRowContext(t.Context(), `SELECT (SELECT value FROM hosted_usage_counters WHERE organization_id=$1 AND name='unarchived_issues'),
	 (SELECT value FROM local_board_populations WHERE organization_id=$1 AND project_id='prj_shared' AND kind='state' AND has_state AND state_name='Todo'),
	 (SELECT duplicate_source_sequence FROM projects WHERE organization_id=$1 AND id='prj_shared')`, organization).Scan(&active, &todo, &sourceAfter); err != nil {
		t.Fatal(err)
	}
	if active != weight+4 || todo != weight+2 || sourceAfter <= sourceBefore {
		t.Fatalf("write accounting after upgrade: active=%d todo=%d source=%d->%d", active, todo, sourceBefore, sourceAfter)
	}
}

func catalogSnapshot(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `
WITH n AS (SELECT oid,nspname FROM pg_namespace WHERE nspname NOT LIKE 'pg\_%' AND nspname<>'information_schema'),
acl(kind,owner,item) AS (
 SELECT 'relation',c.oid,a::text FROM pg_class c, unnest(c.relacl) a
 UNION ALL SELECT 'routine',p.oid,a::text FROM pg_proc p, unnest(p.proacl) a
 UNION ALL SELECT 'schema',s.oid,a::text FROM pg_namespace s, unnest(s.nspacl) a
),
grants AS (SELECT kind,owner,string_agg(item,',' ORDER BY item) AS items FROM acl GROUP BY kind,owner)
SELECT entry FROM (
 SELECT format('schema %s owner=%s acl=%s',n.nspname,pg_get_userbyid(s.nspowner),g.items) AS entry
  FROM n JOIN pg_namespace s ON s.oid=n.oid LEFT JOIN grants g ON g.kind='schema' AND g.owner=s.oid
 UNION ALL SELECT format('relation %s.%s kind=%s owner=%s rls=%s force=%s persistence=%s options=%s acl=%s view=%s',n.nspname,c.relname,c.relkind,pg_get_userbyid(c.relowner),c.relrowsecurity,c.relforcerowsecurity,c.relpersistence,c.reloptions,g.items,CASE WHEN c.relkind IN ('v','m') THEN pg_get_viewdef(c.oid) END)
  FROM pg_class c JOIN n ON n.oid=c.relnamespace LEFT JOIN grants g ON g.kind='relation' AND g.owner=c.oid WHERE c.relkind IN ('r','p','v','m','S','f')
 UNION ALL SELECT format('column %s.%s.%s position=%s type=%s notnull=%s default=%s identity=%s generated=%s collation=%s acl=%s',n.nspname,c.relname,a.attname,
   row_number() OVER (PARTITION BY c.oid ORDER BY a.attnum),format_type(a.atttypid,a.atttypmod),a.attnotnull,pg_get_expr(d.adbin,d.adrelid),a.attidentity,a.attgenerated,
   (SELECT collname FROM pg_collation WHERE oid=a.attcollation),(SELECT string_agg(i::text,',' ORDER BY i::text) FROM unnest(a.attacl) i))
  FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN n ON n.oid=c.relnamespace LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
  WHERE a.attnum>0 AND NOT a.attisdropped AND c.relkind IN ('r','p','v','m','f')
 UNION ALL SELECT format('constraint %s.%s.%s %s validated=%s',n.nspname,c.relname,o.conname,pg_get_constraintdef(o.oid),o.convalidated)
  FROM pg_constraint o JOIN pg_class c ON c.oid=o.conrelid JOIN n ON n.oid=c.relnamespace
 UNION ALL SELECT format('index %s.%s %s valid=%s ready=%s',n.nspname,i.relname,pg_get_indexdef(x.indexrelid),x.indisvalid,x.indisready)
  FROM pg_index x JOIN pg_class i ON i.oid=x.indexrelid JOIN n ON n.oid=i.relnamespace
 UNION ALL SELECT format('trigger %s.%s.%s %s enabled=%s',n.nspname,c.relname,t.tgname,pg_get_triggerdef(t.oid),t.tgenabled)
  FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN n ON n.oid=c.relnamespace WHERE NOT t.tgisinternal
 UNION ALL SELECT format('policy %s.%s.%s %s %s roles=%s using=%s check=%s',p.schemaname,p.tablename,p.policyname,p.permissive,p.cmd,(SELECT string_agg(r::text,',' ORDER BY r::text) FROM unnest(p.roles) r),p.qual,p.with_check)
  FROM pg_policies p JOIN n ON n.nspname=p.schemaname
 UNION ALL SELECT format('routine %s.%s(%s) owner=%s acl=%s %s',n.nspname,p.proname,pg_get_function_identity_arguments(p.oid),pg_get_userbyid(p.proowner),g.items,pg_get_functiondef(p.oid))
  FROM pg_proc p JOIN n ON n.oid=p.pronamespace LEFT JOIN grants g ON g.kind='routine' AND g.owner=p.oid WHERE p.prokind IN ('f','p')
 UNION ALL SELECT format('type %s.%s kind=%s base=%s labels=%s',n.nspname,y.typname,y.typtype,CASE WHEN y.typtype='d' THEN format_type(y.typbasetype,y.typtypmod) END,(SELECT string_agg(e.enumlabel,',' ORDER BY e.enumsortorder) FROM pg_enum e WHERE e.enumtypid=y.oid))
  FROM pg_type y JOIN n ON n.oid=y.typnamespace WHERE y.typtype IN ('e','d','r','m')
 UNION ALL SELECT format('sequence %s.%s type=%s start=%s min=%s max=%s increment=%s cycle=%s',q.schemaname,q.sequencename,q.data_type,q.start_value,q.min_value,q.max_value,q.increment_by,q.cycle)
  FROM pg_sequences q JOIN n ON n.nspname=q.schemaname
 UNION ALL SELECT format('rule %s.%s.%s %s',r.schemaname,r.tablename,r.rulename,r.definition) FROM pg_rules r JOIN n ON n.nspname=r.schemaname
 UNION ALL SELECT format('extension %s version=%s',extname,extversion) FROM pg_extension
 UNION ALL SELECT format('event trigger %s event=%s function=%s enabled=%s tags=%s',evtname,evtevent,evtfoid::regproc,evtenabled,evttags) FROM pg_event_trigger
 UNION ALL SELECT format('default acl role=%s schema=%s object=%s acl=%s',pg_get_userbyid(defaclrole),defaclnamespace::regnamespace,defaclobjtype,(SELECT string_agg(i::text,',' ORDER BY i::text) FROM unnest(defaclacl) i)) FROM pg_default_acl
) catalog ORDER BY entry COLLATE "C"`)
	if err != nil {
		t.Fatal(err)
	}
	var entries []string
	for rows.Next() {
		var entry string
		if err := rows.Scan(&entry); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("catalog snapshot is empty")
	}
	return entries
}

func catalogDifference(want, got []string) string {
	var difference strings.Builder
	report := func(label string, from, other []string) {
		shown := 0
		for _, entry := range from {
			if _, found := slices.BinarySearch(other, entry); found {
				continue
			}
			if shown++; shown > 20 {
				difference.WriteString(label + " ...\n")
				return
			}
			difference.WriteString(label + " " + entry + "\n")
		}
	}
	report("fresh only:", want, got)
	report("upgraded only:", got, want)
	return difference.String()
}
