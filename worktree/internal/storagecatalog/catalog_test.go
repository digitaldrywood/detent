package storagecatalog

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestMain(m *testing.M) {
	if err := testenv.ClearGitEnvironment(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func repositoryCatalog(t *testing.T) Catalog {
	t.Helper()
	catalog, err := Read(os.DirFS("../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestCatalogRejectsInventoryDrift(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*Catalog)
		want   error
	}{
		{"current inventory", func(*Catalog) {}, nil},
		{"missing historical auth column", func(c *Catalog) { delete(c.Tables["auth/sessions"].Columns, "revoked_at") }, ErrUnmapped},
		{"changed historical registry type", func(c *Catalog) {
			col := c.Tables["registry/organizations"].Columns["state"]
			col.Source = "invalid"
			c.Tables["registry/organizations"].Columns["state"] = col
		}, ErrUnmapped},
		{"missing column", func(c *Catalog) { delete(c.Tables["hub/issues"].Columns, "revision") }, ErrUnmapped},
		{"unmapped column", func(c *Catalog) {
			c.Tables["hub/issues"].Columns["unknown"] = c.Tables["hub/issues"].Columns["revision"]
		}, ErrUnmapped},
		{"changed type", func(c *Catalog) {
			col := c.Tables["hub/issues"].Columns["revision"]
			col.Source = `[0,"revision","TEXT",1,null,0,0]`
			c.Tables["hub/issues"].Columns["revision"] = col
		}, ErrUnmapped},
		{"missing table", func(c *Catalog) { delete(c.Tables, "metrics/request_days") }, ErrUnmapped},
		{"wrong owner", func(c *Catalog) {
			table := c.Tables["hub/issues"]
			table.Family = "platform"
			c.Tables["hub/issues"] = table
		}, ErrUnmapped},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog := repositoryCatalog(t)
			test.change(&catalog)
			if err := catalog.ValidateInventory(os.DirFS("../..")); !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
		})
	}
}

func TestCatalogPreservesSafetyFields(t *testing.T) {
	t.Parallel()
	catalog := repositoryCatalog(t)
	for _, test := range []struct {
		table, column, input string
		integer              int64
	}{
		{"hub/issues", "id", "9223372036854775807", math.MaxInt64},
		{"hub/issues", "revision", "9007199254740993", 9007199254740993},
		{"hub/leases", "fencing_token", "9007199254740995", 9007199254740995},
		{"hub/leases", "work_item_revision", "9007199254740997", 9007199254740997},
		{"hub/native_attempts", "dispatch_generation", "9007199254740999", 9007199254740999},
	} {
		t.Run(test.table+"/"+test.column, func(t *testing.T) {
			value, err := catalog.Decode(test.table, test.column, Cell{Encoding: "utf8", Data: []byte(test.input)})
			if err != nil || value.Integer != test.integer {
				t.Fatalf("integer=%d, error=%v", value.Integer, err)
			}
			column := catalog.Tables[test.table].Columns[test.column]
			if len(column.Targets) != 1 || column.Targets[0].Column != test.column {
				t.Fatal("safety field target changed")
			}
		})
	}
	for _, test := range []struct{ table, column, input string }{
		{"hub/leases", "fencing_token", "0"},
		{"hub/issues", "revision", "-1"},
		{"metrics/request_minutes", "count", "0"},
		{"metrics/request_minutes", "status_class", "10"},
		{"metrics/request_minutes", "sum_ms", "-0.5"},
	} {
		if _, err := catalog.Decode(test.table, test.column, Cell{Encoding: "utf8", Data: []byte(test.input)}); !errors.Is(err, ErrValue) {
			t.Fatalf("invalid safety/accounting value accepted for %s/%s: %v", test.table, test.column, err)
		}
	}
	for _, name := range []string{"actor_id", "operation", "command_key", "request_hash"} {
		value, err := catalog.Decode("hub/native_commands", name, Cell{Encoding: "utf8", Data: []byte("exact-receipt-identity")})
		if err != nil || value.Text != "exact-receipt-identity" {
			t.Fatalf("receipt %s changed: %v", name, err)
		}
	}
}

func TestBindingValidatesMappedRowsWithoutClaimingPendingFields(t *testing.T) {
	t.Parallel()
	catalog := repositoryCatalog(t)
	schema := make(Schema)
	for name, table := range catalog.Tables {
		name, ok := strings.CutPrefix(name, "hub/")
		if !ok {
			continue
		}
		schema[name] = make(map[string]string, len(table.Columns))
		for column, definition := range table.Columns {
			schema[name][column] = definition.Source
		}
	}
	binding, err := catalog.Bind(Source{Schema: "raw_example", Store: "hub", CatalogVersion: catalog.Version, OrganizationID: "org_example"}, schema)
	if err != nil {
		t.Fatal(err)
	}
	row := map[string]Cell{
		"id":               {Encoding: "utf8", Data: []byte("invite_example")},
		"organization_id":  {Encoding: "utf8", Data: []byte("org_example")},
		"email":            {Encoding: "utf8", Data: []byte("user@example.test")},
		"role":             {Encoding: "utf8", Data: []byte("viewer")},
		"accepted_user_id": {Encoding: "utf8", Data: []byte("")},
		"created_at":       {Encoding: "utf8", Data: []byte("2026-10-31T12:00:00Z")},
		"expires_at":       {Encoding: "utf8", Data: []byte("2026-11-07T12:00:00Z")},
		"grants_json":      {Encoding: "utf8", Data: []byte(`[{"project_id":"prj_example"}]`)},
	}
	if _, err := binding.ValidateRow("hosted_invitations", row); !errors.Is(err, ErrPending) {
		t.Fatalf("complete row validation error = %v, want pending", err)
	}
	if organization, err := binding.ValidateMappedRow("hosted_invitations", row); err != nil || organization != "org_example" {
		t.Fatalf("mapped row organization=%q error=%v", organization, err)
	}
	row["role"] = Cell{Encoding: "utf8", Data: []byte("unknown")}
	if _, err := binding.ValidateMappedRow("hosted_invitations", row); !errors.Is(err, ErrValue) {
		t.Fatalf("invalid mapped role error = %v, want invalid value", err)
	}
}

func TestValueDecodingRejectsLossAndAmbiguity(t *testing.T) {
	t.Parallel()
	instant := time.Date(2026, 11, 1, 14, 0, 0, 123456000, time.UTC)
	for _, test := range []struct {
		name, codec, input string
		nullPolicy, empty  string
		want               Value
		fail               bool
	}{
		{name: "integer overflow", codec: "int64", input: "9223372036854775808", fail: true},
		{name: "float integer", codec: "int64", input: "1.0", fail: true},
		{name: "scientific integer", codec: "int64", input: "1e3", fail: true},
		{name: "integer whitespace", codec: "int64", input: " 1", fail: true},
		{name: "empty integer", codec: "int64", fail: true},
		{name: "true", codec: "bool01", input: "1", want: Value{Boolean: true}},
		{name: "false", codec: "bool01", input: "0"},
		{name: "nonboolean integer", codec: "bool01", input: "2", fail: true},
		{name: "guessed boolean", codec: "bool01", input: "true", fail: true},
		{name: "nan", codec: "float64", input: "NaN", fail: true},
		{name: "infinity", codec: "float64", input: "Inf", fail: true},
		{name: "hex float", codec: "float64", input: "0x1p2", fail: true},
		{name: "real", codec: "float64", input: "1.25e2", want: Value{Real: 125}},
		{name: "microseconds", codec: "timestamp", input: "2026-11-01T09:00:00.123456-05:00", want: Value{Time: instant}},
		{name: "trailing zero precision", codec: "timestamp", input: "2026-11-01T14:00:00.1234560000Z", want: Value{Time: instant}},
		{name: "nanosecond truncates to target precision", codec: "timestamp", input: "2026-11-01T14:00:00.123456789Z", want: Value{Time: instant}},
		{name: "offset timestamp truncates after UTC conversion", codec: "timestamp", input: "2026-11-01T09:00:00.123456789-05:00", want: Value{Time: instant}},
		{name: "empty no-expiry timestamp maps to finite maximum", codec: "timestamp", empty: "max-timestamp", want: Value{Time: time.Date(9999, time.December, 31, 23, 59, 59, 999999000, time.UTC)}},
		{name: "parser truncation loss", codec: "timestamp", input: "2026-11-01T14:00:00.1234560001Z", fail: true},
		{name: "timestamp timezone missing", codec: "timestamp", input: "2026-11-01 14:00:00", fail: true},
		{name: "bad timestamp", codec: "timestamp", input: "private-malformed-value", fail: true},
		{name: "empty nullable timestamp", codec: "timestamp", empty: "null", want: Value{Null: true}},
		{name: "epoch", codec: "unix-seconds", input: "0", want: Value{Time: time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)}},
		{name: "epoch outside target range", codec: "unix-seconds", input: "9223372036854775807", fail: true},
		{name: "empty text", codec: "text", empty: "preserve", want: Value{Text: "", Bytes: []byte{}}},
		{name: "nul text", codec: "text", input: "private\x00content", fail: true},
		{name: "invalid utf8", codec: "text", input: "\xff", fail: true},
		{name: "json exact bytes", codec: "object-json", input: ` {"n":9007199254740993} `, want: Value{Bytes: []byte(` {"n":9007199254740993} `)}},
		{name: "jsonb exact bytes", codec: "json", input: ` {"n":9007199254740993} `, want: Value{Bytes: []byte(` {"n":9007199254740993} `)}},
		{name: "malformed jsonb", codec: "json", input: `{"private":`, fail: true},
		{name: "malformed payload", codec: "object-json", input: `{"private":`, fail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			column := Column{Codec: test.codec, Null: test.nullPolicy, Empty: test.empty}
			got, err := decode(column, Cell{Encoding: "utf8", Data: []byte(test.input)})
			if test.fail {
				if !errors.Is(err, ErrValue) || strings.Contains(err.Error(), test.input) && test.input != "" {
					t.Fatalf("got %v", err)
				}
			} else if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("got %+v, %v; want %+v", got, err, test.want)
			}
		})
	}
}

func TestBlobEncodingsPreserveEncryptedBytes(t *testing.T) {
	t.Parallel()
	want := []byte{0, 255, 128, 39, 92, 0, 1}
	column := Column{Codec: "bytes", Null: "preserve", Empty: "preserve"}
	for _, test := range []struct {
		encoding string
		data     []byte
		fail     bool
	}{
		{"binary", want, false}, {"hex", []byte("00ff80275c0001"), false}, {"sqlite-hex", []byte("X'00FF80275C0001'"), false},
		{"base64", []byte(base64.StdEncoding.EncodeToString(want)), false},
		{"utf8", want, true}, {"hex", []byte("private-value"), true}, {"hex", []byte("0"), true},
		{"base64", []byte("/x=="), true}, {"base64", []byte("AA==\n"), true},
	} {
		t.Run(test.encoding+string(test.data), func(t *testing.T) {
			value, err := decode(column, Cell{Encoding: test.encoding, Data: test.data})
			if test.fail {
				if !errors.Is(err, ErrValue) {
					t.Fatalf("got %v", err)
				}
				return
			}
			if err != nil || !bytes.Equal(value.Bytes, want) {
				t.Fatalf("blob changed: %v", err)
			}
			if len(value.Bytes) > 0 {
				value.Bytes[0] = 1
				if want[0] != 0 {
					t.Fatal("decoded bytes alias source")
				}
			}
		})
	}
	for _, test := range []struct {
		cell Cell
		fail bool
		null bool
	}{
		{Cell{Null: true}, false, true}, {Cell{Encoding: "binary", Data: []byte{}}, false, false},
		{Cell{Null: true, Data: []byte("secret")}, true, false},
	} {
		got, err := decode(column, test.cell)
		if (err != nil) != test.fail || got.Null != test.null {
			t.Fatalf("null/empty conflated: %+v %v", got, err)
		}
	}
}

func TestHistogramRejectsMalformedBins(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, first string
		size        int
		fail        bool
	}{
		{"valid", "4", 64, false}, {"null bin", "null", 64, true}, {"negative bin", "-1", 64, true}, {"overflow bin", "9223372036854775808", 64, true}, {"short", "0", 63, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := "[" + test.first + strings.Repeat(",0", test.size-1) + "]"
			got, err := decode(Column{Codec: "histogram"}, Cell{Encoding: "utf8", Data: []byte(input)})
			if (err != nil) != test.fail {
				t.Fatalf("got %v", err)
			}
			if !test.fail && (len(got.Histogram) != 64 || got.Histogram[0] != 4) {
				t.Fatal("histogram changed")
			}
		})
	}
}

func TestCatalogReadFailsClosed(t *testing.T) {
	t.Parallel()
	for _, data := range []string{`{}`, `{"version":"1","tables":{},"unknown":true}`, `{"version":"1","tables":{}} {}`, `private-malformed-content`} {
		_, err := Read(fstest.MapFS{"catalog": {Data: []byte(data)}}, "catalog")
		if !errors.Is(err, ErrCatalog) || strings.Contains(err.Error(), "private-malformed-content") {
			t.Fatalf("got %v", err)
		}
	}
	catalog := repositoryCatalog(t)
	wrong := catalog.Tables["hub/issues"].Columns["revision"]
	wrong.Targets = []Target{{Table: "public.issues", Column: "revision", Type: "text"}}
	catalog.Tables["hub/issues"].Columns["revision"] = wrong
	if err := catalog.Validate(); !errors.Is(err, ErrCatalog) {
		t.Fatalf("integer decoder accepted text target: %v", err)
	}
	catalog = repositoryCatalog(t)
	column := catalog.Tables["registry/organizations"].Columns["state"]
	if _, err := decode(column, Cell{Encoding: "utf8", Data: []byte("unsupported-state")}); !errors.Is(err, ErrValue) {
		t.Fatalf("enum accepted: %v", err)
	}
	if _, err := catalog.Decode("hub/projects", "icon_name", Cell{Encoding: "utf8", Data: []byte("secret")}); !errors.Is(err, ErrPending) {
		t.Fatalf("pending column accepted: %v", err)
	}
}

func TestSyntheticSchemaVersionsAndTenantBindings(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"legacy-inline", "current-object"} {
		t.Run(version, func(t *testing.T) {
			column := Column{Source: `[0,"id","TEXT",1,null,1,0]`, Disposition: "column", Codec: "text", Null: "reject", Empty: "reject", Targets: []Target{{Table: "public.projects", Column: "id", Type: "text"}}}
			columns := map[string]Column{"id": column}
			if version == "current-object" {
				columns["organization_id"] = Column{Source: `[1,"organization_id","TEXT",1,null,0,0]`, Disposition: "column", Codec: "text", Null: "reject", Empty: "reject", Targets: []Target{{Table: "public.projects", Column: "organization_id", Type: "text"}}}
			}
			catalog := Catalog{Version: version, Tables: map[string]Table{"hub/projects": {Family: "core", Disposition: "target", Contract: "Preserve exact IDs", Organization: "binding", Columns: columns}}}
			schema := Schema{"projects": {}}
			for name, column := range columns {
				schema["projects"][name] = column.Source
			}
			for _, organization := range []string{"org_example_first", "org_example_second"} {
				binding, err := catalog.Bind(Source{Schema: "raw_example", Store: "hub", CatalogVersion: version, OrganizationID: organization}, schema)
				if err != nil {
					t.Fatal(err)
				}
				row := map[string]Cell{"id": {Encoding: "utf8", Data: []byte("same-id-in-both-tenants")}}
				if version == "current-object" {
					row["organization_id"] = Cell{Encoding: "utf8", Data: []byte(organization)}
				}
				got, err := binding.ValidateRow("projects", row)
				if err != nil || got != organization {
					t.Fatalf("organization=%q: %v", got, err)
				}
				row["organization_id"] = Cell{Encoding: "utf8", Data: []byte("org_unmapped")}
				if _, err := binding.ValidateRow("projects", row); err == nil {
					t.Fatal("cross-tenant or unknown column accepted")
				}
			}
			for _, source := range []Source{
				{Schema: "raw_example", Store: "hub", CatalogVersion: "unknown", OrganizationID: "org_example"},
				{Schema: "raw_example", Store: "hub", CatalogVersion: version},
				{Schema: "raw_example;injected", Store: "hub", CatalogVersion: version, OrganizationID: "org_example"},
			} {
				if _, err := catalog.Bind(source, schema); err == nil {
					t.Fatal("unmapped source accepted")
				}
			}
			schema["projects"]["unmapped"] = "TEXT"
			if _, err := catalog.Bind(Source{Schema: "raw_example", Store: "hub", CatalogVersion: version, OrganizationID: "org_example"}, schema); !errors.Is(err, ErrUnmapped) {
				t.Fatalf("unknown source column accepted: %v", err)
			}
		})
	}
}

func TestPlatformOrganizationsAreExplicit(t *testing.T) {
	t.Parallel()
	full := repositoryCatalog(t)
	table := full.Tables["registry/organizations"]
	catalog := Catalog{Version: full.Version, Tables: map[string]Table{"registry/organizations": table}}
	schema := Schema{"organizations": {}}
	for name, column := range table.Columns {
		schema["organizations"][name] = column.Source
	}
	organizations := []string{"org_example_first", "org_example_second"}
	binding, err := catalog.Bind(Source{Schema: "raw_registry", Store: "registry", CatalogVersion: catalog.Version, Organizations: organizations}, schema)
	if err != nil {
		t.Fatal(err)
	}
	organizations[0] = "org_unmapped"
	for _, organization := range []string{"org_example_first", "org_example_second", "org_unmapped"} {
		row := map[string]Cell{}
		for name, column := range table.Columns {
			text := "example"
			switch column.Codec {
			case "timestamp":
				text = "2026-11-01T00:00:00Z"
			case "int64":
				text = "1"
			case "bool01":
				text = "0"
			}
			if len(column.Allowed) > 0 {
				text = column.Allowed[0]
			}
			row[name] = Cell{Encoding: "utf8", Data: []byte(text)}
		}
		row["id"] = Cell{Encoding: "utf8", Data: []byte(organization)}
		got, err := binding.ValidateRow("organizations", row)
		if organization == "org_unmapped" {
			if !errors.Is(err, ErrOrganization) {
				t.Fatalf("unmapped organization accepted: %v", err)
			}
		} else if err != nil || got != organization {
			t.Fatalf("organization=%q: %v", got, err)
		}
	}
}

func TestCatalogObjectsKeepExactPayloads(t *testing.T) {
	t.Parallel()
	catalog := repositoryCatalog(t)
	raw := []byte(` {"receipt":"exact","revision":9007199254740993} `)
	value, err := catalog.Decode("hub/native_commands", "response_json", Cell{Encoding: "utf8", Data: raw})
	if err != nil || !bytes.Equal(value.Bytes, raw) {
		t.Fatalf("payload changed: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(value.Bytes, &payload); err != nil {
		t.Fatal(err)
	}
	if string(payload["revision"]) != "9007199254740993" {
		t.Fatal("payload number rounded")
	}
}
