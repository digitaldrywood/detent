package searchrecords_test

import (
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
)

const singleScopePostingsVersion = 20261113155409

const staleSearchScopeKeys = `CREATE OR REPLACE FUNCTION public.search_scope_keys(project_id text,work_item_id text,kind text,scope text,status text,archived boolean,principal_id text)
RETURNS SETOF text LANGUAGE sql IMMUTABLE SET search_path=pg_catalog AS $keys$
SELECT DISTINCT public.search_scope_key(p,w,CASE WHEN $3='thread' THEN 'thread' ELSE 'work' END,s,t,a,r)
FROM unnest(CASE WHEN $3='thread' THEN ARRAY['',COALESCE($1,'')] ELSE ARRAY[$1] END) p
CROSS JOIN unnest(ARRAY['',COALESCE($2,'')]) w
CROSS JOIN unnest(ARRAY['',$4]) s
CROSS JOIN unnest(ARRAY['',$5]) t
CROSS JOIN unnest(ARRAY['all',CASE WHEN $6 THEN 'true' ELSE 'false' END]) a
CROSS JOIN unnest(CASE WHEN $3='thread' THEN ARRAY[''] ELSE ARRAY['',COALESCE($7,'')] END) r
$keys$`

func TestSearchPostingsMigrationGuardAndOrphans(t *testing.T) {
	t.Parallel()
	f := newSearchFixture(t)
	d := f.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Title: "migrationterm"}, 0)
	f.drain(t, "org_first")
	f.admin.SetMaxOpenConns(1)
	provider, err := pgstoretest.MigrationProvider(f.admin)
	if err != nil {
		t.Fatal(err)
	}
	asMigration := func(apply func() error) error {
		t.Helper()
		if _, err := f.admin.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
			t.Fatal(err)
		}
		if _, err := f.admin.ExecContext(t.Context(), `SET detent.organization_id=''`); err != nil {
			t.Fatal(err)
		}
		applyErr := apply()
		if _, err := f.admin.ExecContext(t.Context(), `RESET ROLE`); err != nil {
			t.Fatal(err)
		}
		return applyErr
	}
	if err := asMigration(func() error {
		_, err := provider.ApplyVersion(t.Context(), singleScopePostingsVersion, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(t.Context(), `ALTER TABLE search_documents DISABLE TRIGGER ALL`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO search_documents(organization_id,id,project_id,work_item_id,kind,source_id,revision,indexed_revision,title,scope,updated_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
 VALUES('org_first','issue/orphan','prj_missing','wi_orphan','issue','wi_orphan',1,1,'migrationterm','issue',CURRENT_TIMESTAMP,$1,$2,$3,$4)`, d.Payload.Key, d.Payload.SHA256, d.Payload.CompressedSize, d.Payload.UncompressedSize); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(t.Context(), `ALTER TABLE search_documents ENABLE TRIGGER ALL`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO search_postings(organization_id,scope,term,document_id,impact,updated_at,updated_at_submicrosecond) VALUES('org_first','legacy-scope','legacy-term',$1,1,CURRENT_TIMESTAMP,0)`, d.ID); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		t.Helper()
		var state string
		if err := f.admin.QueryRowContext(t.Context(), `SELECT (SELECT string_agg(column_name,',' ORDER BY column_name) FROM information_schema.columns WHERE table_schema='public' AND table_name='search_postings')
 ||'|'||(SELECT md5(prosrc) FROM pg_proc WHERE oid='public.search_document_postings()'::regprocedure)
 ||'|'||(SELECT md5(string_agg(organization_id||scope||term||document_id,',' ORDER BY organization_id,scope,term,document_id)) FROM search_postings)`).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	before := snapshot()
	if _, err := f.admin.ExecContext(t.Context(), staleSearchScopeKeys); err != nil {
		t.Fatal(err)
	}
	err = asMigration(func() error {
		_, err := provider.ApplyVersion(t.Context(), singleScopePostingsVersion, true)
		return err
	})
	var refused *pgconn.PgError
	if !errors.As(err, &refused) || refused.Code != "P0001" {
		t.Fatalf("stale roles migration error=%v", err)
	}
	if after := snapshot(); after != before {
		t.Fatalf("refused migration changed old table, trigger or data: before=%s after=%s", before, after)
	}
	roles, err := os.ReadFile("../roles.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(t.Context(), string(roles)); err != nil {
		t.Fatal(err)
	}
	if err := asMigration(func() error {
		_, err := provider.ApplyVersion(t.Context(), singleScopePostingsVersion, true)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var issueMarkers, orphanMarkers, orphanRefresh int
	if err := f.admin.QueryRowContext(t.Context(), `SELECT count(*) FILTER(WHERE document_id=$1),count(*) FILTER(WHERE document_id='issue/orphan'),
 (SELECT count(*) FROM search_scope_refresh WHERE organization_id='org_first' AND project_id='prj_missing')
 FROM search_postings WHERE organization_id='org_first' AND term IN ('~all','~pending')`, d.ID).Scan(&issueMarkers, &orphanMarkers, &orphanRefresh); err != nil {
		t.Fatal(err)
	}
	if issueMarkers != 2 || orphanMarkers != 0 || orphanRefresh != 0 {
		t.Fatalf("issue markers=%d orphan markers=%d orphan refresh=%d", issueMarkers, orphanMarkers, orphanRefresh)
	}
}
