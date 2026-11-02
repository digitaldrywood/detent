package storagecatalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"
)

var (
	ErrCatalog      = errors.New("invalid source catalog")
	ErrUnmapped     = errors.New("unmapped source schema")
	ErrValue        = errors.New("unsupported source value")
	ErrPending      = errors.New("target mapping pending")
	ErrOrganization = errors.New("unmapped source organization")
)

type Catalog struct {
	Version string           `json:"version"`
	Tables  map[string]Table `json:"tables"`
}

type Table struct {
	Family       string            `json:"family"`
	Disposition  string            `json:"disposition"`
	Contract     string            `json:"contract"`
	Organization string            `json:"organization"`
	Columns      map[string]Column `json:"columns"`
}

type Column struct {
	Source      string   `json:"source"`
	Disposition string   `json:"disposition"`
	Transform   string   `json:"transform,omitempty"`
	Codec       string   `json:"codec"`
	Null        string   `json:"null"`
	Empty       string   `json:"empty"`
	Targets     []Target `json:"targets,omitempty"`
	Reason      string   `json:"reason,omitempty"`
	Allowed     []string `json:"allowed,omitempty"`
	Minimum     *int64   `json:"minimum,omitempty"`
	Maximum     *int64   `json:"maximum,omitempty"`
	Nonnegative bool     `json:"nonnegative,omitempty"`
}

type Target struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	Type   string `json:"type"`
}

func Read(files fs.FS, name string) (Catalog, error) {
	var catalog Catalog
	data, err := fs.ReadFile(files, name)
	if err != nil {
		return catalog, fmt.Errorf("read source catalog: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&catalog); err != nil {
		return Catalog{}, ErrCatalog
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return Catalog{}, ErrCatalog
	}
	if err := catalog.Validate(); err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

func (c Catalog) Validate() error {
	if c.Version == "" || len(c.Tables) == 0 {
		return ErrCatalog
	}
	for name, table := range c.Tables {
		store, tableName, ok := strings.Cut(name, "/")
		if !ok || !slices.Contains([]string{"hub", "metrics", "registry", "auth"}, store) || !identifier(tableName) || table.Family == "" || table.Contract == "" || len(table.Columns) == 0 {
			return ErrCatalog
		}
		if !slices.Contains([]string{"target", "rebuild", "source-only"}, table.Disposition) || !slices.Contains([]string{"row", "binding", "platform"}, table.Organization) {
			return ErrCatalog
		}
		for name, column := range table.Columns {
			if column.Minimum != nil && column.Maximum != nil && *column.Minimum > *column.Maximum {
				return ErrCatalog
			}
			if !identifier(name) || column.Source == "" || !slices.Contains([]string{"preserve", "reject"}, column.Null) || !slices.Contains([]string{"preserve", "null", "reject"}, column.Empty) {
				return ErrCatalog
			}
			if !slices.Contains([]string{"text", "int32", "int64", "float64", "bool01", "timestamp", "unix-seconds", "bytes", "object-text", "object-json", "json", "histogram"}, column.Codec) {
				return ErrCatalog
			}
			if !slices.Contains([]string{"column", "object", "transform", "pending", "rebuild", "retired"}, column.Disposition) {
				return ErrCatalog
			}
			if slices.Contains([]string{"column", "object"}, column.Disposition) && len(column.Targets) == 0 {
				return ErrCatalog
			}
			if column.Disposition == "column" && !scalarTargetMatches(column) {
				return ErrCatalog
			}
			if column.Disposition == "object" && (!slices.Contains([]string{"object-text", "object-json"}, column.Codec) || column.Targets[0].Type != "text") {
				return ErrCatalog
			}
			if slices.Contains([]string{"pending", "rebuild", "retired"}, column.Disposition) && (column.Reason == "" || len(column.Targets) != 0 || column.Transform != "") {
				return ErrCatalog
			}
			if column.Disposition == "transform" && (!validTransform(column.Transform) || column.Reason != "" || len(column.Targets) == 0 && column.Transform != "tenant-singleton") {
				return ErrCatalog
			}
			if column.Disposition != "transform" && column.Transform != "" {
				return ErrCatalog
			}
			if table.Disposition == "source-only" && column.Disposition != "retired" || table.Disposition == "rebuild" && column.Disposition != "rebuild" {
				return ErrCatalog
			}
			for _, target := range column.Targets {
				schema, name, ok := strings.Cut(target.Table, ".")
				if !ok || !slices.Contains([]string{"public", "platform"}, schema) || !identifier(name) || !identifier(target.Column) || !slices.Contains([]string{"text", "integer", "bigint", "boolean", "timestamp with time zone", "double precision", "bytea", "jsonb", "ARRAY", "numeric"}, target.Type) {
					return ErrCatalog
				}
			}
		}
	}
	return nil
}

func validTransform(value string) bool {
	return slices.Contains([]string{"archive-json", "conversation-attachment", "cost-observation", "hosted-identity", "invitation-grants", "provider-binding", "tenant-singleton"}, value)
}

func scalarTargetMatches(column Column) bool {
	if len(column.Targets) != 1 {
		return false
	}
	var expected string
	switch column.Codec {
	case "text":
		expected = "text"
	case "int64":
		expected = "bigint"
	case "int32":
		expected = "integer"
	case "float64":
		expected = "double precision"
	case "json":
		expected = "jsonb"
	case "bool01":
		expected = "boolean"
	case "timestamp", "unix-seconds":
		expected = "timestamp with time zone"
	case "bytes":
		expected = "bytea"
	case "histogram":
		expected = "ARRAY"
	}
	return expected != "" && column.Targets[0].Type == expected
}

func identifier(value string) bool {
	if value == "" {
		return false
	}
	for i, ch := range value {
		if ch != '_' && (ch < 'a' || ch > 'z') && (i == 0 || ch < '0' || ch > '9') {
			return false
		}
	}
	return true
}

type Schema map[string]map[string]string

func (c Catalog) ValidateSchema(store string, schema Schema) error {
	if !slices.Contains([]string{"hub", "metrics", "registry", "auth"}, store) {
		return ErrUnmapped
	}
	for name, columns := range schema {
		table, ok := c.Tables[store+"/"+name]
		if !ok || len(columns) != len(table.Columns) {
			return ErrUnmapped
		}
		for name, source := range columns {
			column, ok := table.Columns[name]
			if !ok || source != column.Source {
				return ErrUnmapped
			}
		}
	}
	for name := range c.Tables {
		if suffix, ok := strings.CutPrefix(name, store+"/"); ok {
			if _, exists := schema[suffix]; !exists {
				return ErrUnmapped
			}
		}
	}
	return nil
}

func (c Catalog) ValidateInventory(files fs.FS) error {
	var routing struct {
		Tables map[string]struct{ Family, Disposition, Contract string } `json:"tables"`
	}
	data, err := fs.ReadFile(files, "tools/storageinventory/routing.json")
	if err != nil {
		return fmt.Errorf("read storage routing: %w", err)
	}
	if json.Unmarshal(data, &routing) != nil || len(routing.Tables) != len(c.Tables) {
		return ErrUnmapped
	}
	for name, route := range routing.Tables {
		table, ok := c.Tables[name]
		if !ok || table.Family != route.Family || table.Disposition != route.Disposition || table.Contract != route.Contract {
			return ErrUnmapped
		}
	}
	for _, source := range []struct{ store, directory string }{{"hub", "hubserver"}, {"metrics", "hubserver"}, {"registry", "cloudentry"}, {"auth", "cloudentry"}} {
		data, err := fs.ReadFile(files, "internal/"+source.directory+"/testdata/"+source.store+"_schema.json")
		if err != nil {
			return fmt.Errorf("read source schema: %w", err)
		}
		var snapshot map[string]string
		if json.Unmarshal(data, &snapshot) != nil {
			return ErrUnmapped
		}
		schema := make(Schema)
		for key, value := range snapshot {
			if column, ok := strings.CutPrefix(key, "column/"); ok {
				table, name, ok := strings.Cut(column, "/")
				if !ok {
					return ErrUnmapped
				}
				if schema[table] == nil {
					schema[table] = make(map[string]string)
				}
				schema[table][name] = value
			}
		}
		if err := c.ValidateSchema(source.store, schema); err != nil {
			return err
		}
	}
	return nil
}
