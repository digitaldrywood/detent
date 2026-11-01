package postgres_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

func TestSourceCatalogTargetsMatchPostgres(t *testing.T) {
	db := template.Database(t)
	catalog, err := storagecatalog.Read(os.DirFS("../../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(t.Context(), `SELECT table_schema || '.' || table_name, column_name, data_type FROM information_schema.columns WHERE table_schema IN ('public','platform')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	targets := map[string]string{}
	for rows.Next() {
		var table, column, kind string
		if err := rows.Scan(&table, &column, &kind); err != nil {
			t.Fatal(err)
		}
		targets[table+"."+column] = kind
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for tableName, table := range catalog.Tables {
		for columnName, column := range table.Columns {
			for _, target := range column.Targets {
				if got := targets[target.Table+"."+target.Column]; got != target.Type {
					t.Errorf("%s/%s: target %s.%s has type %q, catalog requires %q", tableName, columnName, target.Table, target.Column, got, target.Type)
				}
			}
		}
	}
}

func TestSourceCatalogSyntheticRawSchemas(t *testing.T) {
	db := template.Database(t)
	for _, statement := range []string{
		`CREATE SCHEMA raw_legacy_fixture`,
		`CREATE SCHEMA raw_current_fixture`,
		`CREATE TABLE raw_legacy_fixture.records (id text, revision text, enabled text, recorded_at text, ciphertext bytea, payload text)`,
		`CREATE TABLE raw_current_fixture.records (organization_id text, id text, revision text, enabled text, recorded_at text, ciphertext bytea, payload text)`,
	} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	encrypted := []byte{0, 255, 128, 39, 92, 0}
	payload := ` {"receipt":"same-receipt","fence":9007199254740993} `
	for _, organization := range []string{"org_example_first", "org_example_second"} {
		for _, statement := range []string{
			`INSERT INTO raw_legacy_fixture.records VALUES ($1,$2,$3,$4,$5,$6)`,
			`INSERT INTO raw_current_fixture.records VALUES ($7,$1,$2,$3,$4,$5,$6)`,
		} {
			args := []any{"same-id", "9007199254740993", "1", "2026-11-01T09:00:00.123456-05:00", encrypted, payload}
			if strings.Contains(statement, "$7") {
				args = append(args, organization)
			}
			if _, err := db.ExecContext(t.Context(), statement, args...); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, version := range []string{"legacy", "current"} {
		t.Run(version, func(t *testing.T) {
			columns := map[string]storagecatalog.Column{}
			for name, codec := range map[string]string{"id": "text", "revision": "int64", "enabled": "bool01", "recorded_at": "timestamp", "ciphertext": "bytes", "payload": "object-json"} {
				disposition := "column"
				if name == "payload" {
					disposition = "object"
				}
				kind := map[string]string{"text": "text", "int64": "bigint", "bool01": "boolean", "timestamp": "timestamp with time zone", "bytes": "bytea", "object-json": "text"}[codec]
				columns[name] = storagecatalog.Column{Source: name + "-synthetic-v1", Disposition: disposition, Codec: codec, Null: "reject", Empty: "reject", Targets: []storagecatalog.Target{{Table: "public.synthetic_fixture", Column: name, Type: kind}}}
			}
			if version == "current" {
				columns["organization_id"] = storagecatalog.Column{Source: "organization-synthetic-v1", Disposition: "column", Codec: "text", Null: "reject", Empty: "reject", Targets: []storagecatalog.Target{{Table: "public.synthetic_fixture", Column: "organization_id", Type: "text"}}}
			}
			catalog := storagecatalog.Catalog{Version: version, Tables: map[string]storagecatalog.Table{"hub/records": {Family: "core", Disposition: "target", Contract: "Synthetic raw types preserve IDs and encrypted bytes", Organization: "binding", Columns: columns}}}
			schema := storagecatalog.Schema{"records": {}}
			for name, column := range columns {
				schema["records"][name] = column.Source
			}
			query := `SELECT id,revision,enabled,recorded_at,ciphertext,payload FROM raw_legacy_fixture.records`
			if version == "current" {
				query = `SELECT id,revision,enabled,recorded_at,ciphertext,payload,organization_id FROM raw_current_fixture.records`
			}
			rows, err := db.QueryContext(t.Context(), query)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			seen := 0
			for rows.Next() {
				var id, revision, enabled, recordedAt, rawPayload, organization string
				var ciphertext []byte
				destinations := []any{&id, &revision, &enabled, &recordedAt, &ciphertext, &rawPayload}
				if version == "current" {
					destinations = append(destinations, &organization)
				} else {
					organization = "org_example_first"
				}
				if err := rows.Scan(destinations...); err != nil {
					t.Fatal(err)
				}
				binding, err := catalog.Bind(storagecatalog.Source{Schema: "raw_" + version + "_fixture", Store: "hub", CatalogVersion: version, OrganizationID: organization}, schema)
				if err != nil {
					t.Fatal(err)
				}
				raw := map[string]storagecatalog.Cell{}
				for name, text := range map[string]string{"id": id, "revision": revision, "enabled": enabled, "recorded_at": recordedAt, "payload": rawPayload} {
					raw[name] = storagecatalog.Cell{Encoding: "utf8", Data: []byte(text)}
				}
				raw["ciphertext"] = storagecatalog.Cell{Encoding: "binary", Data: ciphertext}
				if version == "current" {
					raw["organization_id"] = storagecatalog.Cell{Encoding: "utf8", Data: []byte(organization)}
				}
				got, err := binding.ValidateRow("records", raw)
				if err != nil || got != organization {
					t.Fatalf("organization=%q: %v", got, err)
				}
				for name, cell := range raw {
					value, err := catalog.Decode("hub/records", name, cell)
					if err != nil {
						t.Fatal(err)
					}
					switch name {
					case "id":
						if value.Text != "same-id" {
							t.Fatal("ID changed")
						}
					case "revision":
						if value.Integer != 9007199254740993 {
							t.Fatal("revision rounded")
						}
					case "enabled":
						if !value.Boolean {
							t.Fatal("boolean changed")
						}
					case "ciphertext":
						if !bytes.Equal(value.Bytes, encrypted) {
							t.Fatal("encrypted bytes changed")
						}
					case "payload":
						if !bytes.Equal(value.Bytes, []byte(payload)) {
							t.Fatal("payload changed")
						}
					case "recorded_at":
						if !value.Time.Equal(time.Date(2026, 11, 1, 14, 0, 0, 123456000, time.UTC)) {
							t.Fatal("timestamp changed")
						}
					}
				}
				for _, malformed := range []struct{ name, encoding, data string }{
					{"revision", "utf8", "9223372036854775808"}, {"enabled", "utf8", "2"}, {"recorded_at", "utf8", "2026-11-01T00:00:00.000000001Z"}, {"ciphertext", "hex", hex.EncodeToString(encrypted) + "x"}, {"payload", "utf8", `{"private":`},
				} {
					if _, err := catalog.Decode("hub/records", malformed.name, storagecatalog.Cell{Encoding: malformed.encoding, Data: []byte(malformed.data)}); !errors.Is(err, storagecatalog.ErrValue) {
						t.Fatalf("malformed %s accepted: %v", malformed.name, err)
					}
				}
				seen++
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if seen != 2 {
				t.Fatalf("read %d synthetic rows, want both tenants", seen)
			}
		})
	}
}
