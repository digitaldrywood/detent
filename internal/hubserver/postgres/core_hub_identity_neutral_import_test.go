package postgres_test

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNeutralHubIdentityImportPreservesBinaryCursor(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	objects := newHistoryObjects()
	var wants []accessstore.HubIdentity
	for index, encoding := range []string{"binary", "hex", "base64"} {
		for tenant := 1; tenant <= 2; tenant++ {
			org := tracker.OrganizationID(fmt.Sprintf("org_%s_%d", encoding, tenant))
			seedTenantAccess(t, repository, org)
			key := bytes.Repeat([]byte{0, 0xff, 0x80, byte(tenant)}, 8)
			id := fmt.Sprintf("hub_%032x", 2*index+tenant)
			table := neutralFixtureTable(t, "hub_identity", []postgres.NeutralImportKey{{Column: "id", Kind: "text"}})
			row := neutralFixtureRow(t, table, map[string]any{"id": id, "cursor_key": key})
			switch encoding {
			case "hex":
				row.Values["cursor_key"] = storagecatalog.Cell{Encoding: encoding, Data: []byte(hex.EncodeToString(key))}
			case "base64":
				row.Values["cursor_key"] = storagecatalog.Cell{Encoding: encoding, Data: []byte(base64.StdEncoding.EncodeToString(key))}
			}
			path, _ := writeNeutralFixture(t, org, []postgres.NeutralImportTable{table}, map[string][]postgres.NeutralImportRow{"hub_identity": {row}})
			source := postgres.CoreHubIdentityImportSource{BundlePath: path, OrganizationID: org, Objects: objects}
			for replay := 0; replay < 2; replay++ {
				count, err := repository.ImportCoreHubIdentity(t.Context(), source)
				if err != nil || count != 1 {
					t.Fatalf("%s import=%d: %v", org, count, err)
				}
			}
			changed := row
			changed.Values = map[string]storagecatalog.Cell{"id": row.Values["id"], "cursor_key": {Encoding: "binary", Data: bytes.Repeat([]byte{1}, 32)}}
			changedPath, _ := writeNeutralFixture(t, org, []postgres.NeutralImportTable{table}, map[string][]postgres.NeutralImportRow{"hub_identity": {changed}})
			source.BundlePath = changedPath
			if _, err := repository.ImportCoreHubIdentity(t.Context(), source); !errors.Is(err, mutation.ErrConflict) {
				t.Fatalf("changed manifest accepted: %v", err)
			}
			wants = append(wants, accessstore.HubIdentity{OrganizationID: org, ID: id, CursorKey: key})
		}
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository, _ = repositoryFor(t, admin, smallPool())
	for _, want := range wants {
		if err := repository.ReadTenant(t.Context(), want.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			got, err := r.HubIdentity(t.Context())
			if err != nil {
				return err
			}
			if got.OrganizationID != want.OrganizationID || got.ID != want.ID || !bytes.Equal(got.CursorKey, want.CursorKey) {
				t.Fatalf("reopened identity differs: %+v", got)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNeutralHubIdentityImportRejectsAmbiguousSource(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	objects := newHistoryObjects()
	for _, tc := range []struct {
		name   string
		change func(*postgres.NeutralImportTable, *[]postgres.NeutralImportRow)
	}{
		{"empty", func(_ *postgres.NeutralImportTable, rows *[]postgres.NeutralImportRow) { *rows = nil }},
		{"multiple", func(_ *postgres.NeutralImportTable, rows *[]postgres.NeutralImportRow) {
			extra := (*rows)[0]
			extra.Key = []string{"hub_00000000000000000000000000000002"}
			extra.Values = map[string]storagecatalog.Cell{"id": {Encoding: "utf8", Data: []byte(extra.Key[0])}, "cursor_key": extra.Values["cursor_key"]}
			*rows = append(*rows, extra)
		}},
		{"schema", func(table *postgres.NeutralImportTable, _ *[]postgres.NeutralImportRow) {
			table.Columns["cursor_key"] = `[1,"cursor_key","TEXT",1,null,0,0]`
		}},
		{"identity", func(_ *postgres.NeutralImportTable, rows *[]postgres.NeutralImportRow) {
			(*rows)[0].Key = []string{"invalid"}
			(*rows)[0].Values["id"] = storagecatalog.Cell{Encoding: "utf8", Data: []byte("invalid")}
		}},
		{"cursor_length", func(_ *postgres.NeutralImportTable, rows *[]postgres.NeutralImportRow) {
			(*rows)[0].Values["cursor_key"] = storagecatalog.Cell{Encoding: "binary", Data: []byte{0, 1}}
		}},
		{"cursor_null", func(_ *postgres.NeutralImportTable, rows *[]postgres.NeutralImportRow) {
			(*rows)[0].Values["cursor_key"] = storagecatalog.Cell{Null: true}
		}},
		{"cursor_text", func(_ *postgres.NeutralImportTable, rows *[]postgres.NeutralImportRow) {
			(*rows)[0].Values["cursor_key"] = storagecatalog.Cell{Encoding: "utf8", Data: bytes.Repeat([]byte{'a'}, 32)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			org := tracker.OrganizationID("org_" + tc.name)
			seedTenantAccess(t, repository, org)
			table := neutralFixtureTable(t, "hub_identity", []postgres.NeutralImportKey{{Column: "id", Kind: "text"}})
			rows := []postgres.NeutralImportRow{neutralFixtureRow(t, table, map[string]any{"id": "hub_00000000000000000000000000000001", "cursor_key": bytes.Repeat([]byte{0xff}, 32)})}
			tc.change(&table, &rows)
			path, _ := writeNeutralFixture(t, org, []postgres.NeutralImportTable{table}, map[string][]postgres.NeutralImportRow{"hub_identity": rows})
			if count, err := repository.ImportCoreHubIdentity(t.Context(), postgres.CoreHubIdentityImportSource{BundlePath: path, OrganizationID: org, Objects: objects}); err == nil || count != 0 {
				t.Fatalf("invalid identity import=%d: %v", count, err)
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				_, err := r.HubIdentity(t.Context())
				return err
			}); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("invalid source wrote an identity: %v", err)
			}
		})
	}
	org := tracker.OrganizationID("org_existing")
	seedTenantAccess(t, repository, org)
	want := accessstore.HubIdentity{OrganizationID: org, ID: "hub_00000000000000000000000000000001", CursorKey: bytes.Repeat([]byte{1}, 32)}
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.CreateHubIdentity(t.Context(), want) }); err != nil {
		t.Fatal(err)
	}
	table := neutralFixtureTable(t, "hub_identity", []postgres.NeutralImportKey{{Column: "id", Kind: "text"}})
	row := neutralFixtureRow(t, table, map[string]any{"id": want.ID, "cursor_key": bytes.Repeat([]byte{2}, 32)})
	path, _ := writeNeutralFixture(t, org, []postgres.NeutralImportTable{table}, map[string][]postgres.NeutralImportRow{"hub_identity": {row}})
	if _, err := repository.ImportCoreHubIdentity(t.Context(), postgres.CoreHubIdentityImportSource{BundlePath: path, OrganizationID: org, Objects: objects}); !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("destination identity replaced: %v", err)
	}
	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		got, err := r.HubIdentity(t.Context())
		if err == nil && (got.ID != want.ID || !bytes.Equal(got.CursorKey, want.CursorKey)) {
			t.Fatal("destination cursor changed after refusal")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
