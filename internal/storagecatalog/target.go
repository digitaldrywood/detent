package storagecatalog

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"
)

type TargetCatalog struct {
	Version          string                 `json:"version"`
	LegacyMigrations map[string]string      `json:"legacy_migrations"`
	Migrations       []string               `json:"migrations"`
	Tables           map[string]TargetTable `json:"tables"`
}

type TargetTable struct {
	Family       string            `json:"family"`
	Contract     string            `json:"contract"`
	Organization string            `json:"organization"`
	Columns      map[string]string `json:"columns"`
}

func ReadTargetCatalog(files fs.FS, name string) (TargetCatalog, error) {
	var catalog TargetCatalog
	data, err := fs.ReadFile(files, name)
	if err != nil {
		return catalog, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&catalog); err != nil {
		return TargetCatalog{}, ErrCatalog
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return TargetCatalog{}, ErrCatalog
	}
	if err := catalog.Validate(); err != nil {
		return TargetCatalog{}, err
	}
	return catalog, nil
}

func (c TargetCatalog) Validate() error {
	if c.Version == "" || len(c.Tables) > 0 && len(c.Migrations) == 0 {
		return ErrCatalog
	}
	for migration, digest := range c.LegacyMigrations {
		if !postgresMigration(migration) || !validSHA256(digest) {
			return ErrCatalog
		}
	}
	for _, migration := range c.Migrations {
		if !postgresMigration(migration) {
			return ErrCatalog
		}
	}
	for tableName, table := range c.Tables {
		schema, name, ok := strings.Cut(tableName, ".")
		if !ok || !identifier(schema) || !identifier(name) || table.Family == "" || strings.TrimSpace(table.Contract) == "" || table.Organization != "organization-owned" && table.Organization != "global" || len(table.Columns) == 0 {
			return ErrCatalog
		}
		for column, kind := range table.Columns {
			if !identifier(column) || !slicesContainsTargetType(kind) {
				return ErrCatalog
			}
		}
	}
	return nil
}

var targetCreateTable = regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?("[a-zA-Z_][a-zA-Z_0-9]*"|[a-zA-Z_][a-zA-Z_0-9]*)(?:\s*\.\s*("[a-zA-Z_][a-zA-Z_0-9]*"|[a-zA-Z_][a-zA-Z_0-9]*))?`)
var targetCreateTableToken = regexp.MustCompile(`(?i)\bCREATE\s+TABLE\b`)

func (c TargetCatalog) ValidateMigrations(files fs.FS, sourceTables map[string]json.RawMessage) error {
	created := make(map[string]bool)
	migrationRoot := "internal/hubserver/postgres/migrations"
	entries, err := fs.ReadDir(files, migrationRoot)
	if err != nil {
		return err
	}
	covered := make(map[string]bool, len(c.Migrations))
	var catalogEnd string
	for _, migration := range c.Migrations {
		covered[migration] = true
		if migration > catalogEnd {
			catalogEnd = migration
		}
	}
	legacySeen := make(map[string]bool, len(c.LegacyMigrations))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		migration := migrationRoot + "/" + entry.Name()
		data, err := fs.ReadFile(files, migration)
		if err != nil {
			return err
		}
		if expected, ok := c.LegacyMigrations[migration]; ok {
			actual := fmt.Sprintf("%x", sha256.Sum256(data))
			if actual != expected {
				return errors.Join(ErrCatalog, errors.New("legacy PostgreSQL migration digest changed"))
			}
			legacySeen[migration] = true
			continue
		}
		if !covered[migration] {
			if catalogEnd != "" && migration > catalogEnd {
				continue
			}
			return errors.Join(ErrCatalog, errors.New("new PostgreSQL migration is missing target schema coverage"))
		}
		delete(covered, migration)
		matches := targetCreateTable.FindAllSubmatch(data, -1)
		if len(matches) != len(targetCreateTableToken.FindAll(data, -1)) {
			return errors.Join(ErrCatalog, errors.New("PostgreSQL migration has an unrecognized CREATE TABLE declaration"))
		}
		for _, match := range matches {
			schema, name := "public", strings.Trim(string(match[1]), `"`)
			if len(match[2]) != 0 {
				schema, name = strings.Trim(string(match[1]), `"`), strings.Trim(string(match[2]), `"`)
			}
			if schema == "public" {
				_, target := c.Tables["public."+name]
				if _, exists := sourceTables["hub/"+name]; exists && !target {
					continue
				}
			}
			created[schema+"."+name] = true
		}
	}
	if len(covered) != 0 || len(legacySeen) != len(c.LegacyMigrations) {
		return errors.Join(ErrCatalog, errors.New("PostgreSQL migration coverage has stale entries"))
	}
	if len(created) != len(c.Tables) {
		return errors.Join(ErrCatalog, errors.New("target-only migration table coverage differs from target schema catalog"))
	}
	for table := range created {
		if _, exists := c.Tables[table]; !exists {
			return errors.Join(ErrCatalog, errors.New("target-only migration table is missing from target schema catalog"))
		}
	}
	return nil
}

func (c TargetCatalog) ValidateClassification(classification map[string]TargetClassification) error {
	if len(c.Tables) != len(classification) {
		return errors.Join(ErrCatalog, errors.New("target classification coverage differs from target schema catalog"))
	}
	for target, table := range c.Tables {
		classification, ok := classification[strings.ReplaceAll(target, ".", "/")]
		if !ok || classification.Ownership != table.Organization || strings.TrimSpace(classification.Scope) == "" || len(classification.InlinePayloads) != 0 {
			return ErrCatalog
		}
	}
	return nil
}

func postgresMigration(name string) bool {
	return strings.HasPrefix(name, "internal/hubserver/postgres/migrations/") && strings.HasSuffix(name, ".sql")
}

func validSHA256(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	for _, r := range digest {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

type TargetClassification struct {
	Ownership      string   `json:"ownership"`
	Scope          string   `json:"scope"`
	InlinePayloads []string `json:"inline_payloads"`
}

func slicesContainsTargetType(kind string) bool {
	for _, allowed := range []string{"text", "integer", "bigint", "boolean", "timestamp with time zone", "double precision", "tsvector", "text[]", "bigint[]", "bytea"} {
		if kind == allowed {
			return true
		}
	}
	return false
}
