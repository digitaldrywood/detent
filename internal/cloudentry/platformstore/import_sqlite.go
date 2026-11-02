package platformstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

const (
	RegistryCopyApplicationID = 0x44545247
	AuthCopyApplicationID     = 0x44544155
)

type importCopyVerifier interface {
	Verify(context.Context, bool) error
}

type SQLiteCopySource struct {
	source     ImportSource
	registryDB *sql.DB
	authDB     *sql.DB
	files      []*sealedSQLiteCopy
}

type sealedSQLiteCopy struct {
	path   string
	file   *os.File
	info   os.FileInfo
	digest [32]byte
}

type sqliteCopyVerifier struct {
	copies []*sealedSQLiteCopy
}

func OpenSQLiteCopySource(ctx context.Context, registryPath, authPath string, catalog storagecatalog.Catalog) (*SQLiteCopySource, error) {
	if err := catalog.Validate(); err != nil {
		return nil, err
	}
	registry, registryCopy, err := openSealedSQLiteCopy(ctx, registryPath, RegistryCopyApplicationID)
	if err != nil {
		return nil, fmt.Errorf("open registry copy: %w", err)
	}
	copySource := &SQLiteCopySource{registryDB: registry, files: []*sealedSQLiteCopy{registryCopy}}
	closeOnError := func(err error) (*SQLiteCopySource, error) {
		return nil, errors.Join(err, copySource.Close())
	}
	auth, authCopy, err := openSealedSQLiteCopy(ctx, authPath, AuthCopyApplicationID)
	if err != nil {
		return closeOnError(fmt.Errorf("open auth copy: %w", err))
	}
	copySource.authDB = auth
	copySource.files = append(copySource.files, authCopy)
	registrySchema, err := sqliteCatalogSchema(catalog, "registry")
	if err != nil {
		return closeOnError(fmt.Errorf("bind registry catalog: %w", err))
	}
	authSchema, err := sqliteCatalogSchema(catalog, "auth")
	if err != nil {
		return closeOnError(fmt.Errorf("bind auth catalog: %w", err))
	}
	if err := validateSQLiteStore(ctx, registry, catalog, "registry", registrySchema); err != nil {
		return closeOnError(fmt.Errorf("validate registry copy: %w", err))
	}
	if err := validateSQLiteStore(ctx, auth, catalog, "auth", authSchema); err != nil {
		return closeOnError(fmt.Errorf("validate auth copy: %w", err))
	}
	organizations, err := sqliteOrganizations(ctx, registry)
	if err != nil {
		return closeOnError(fmt.Errorf("read copied organizations: %w", err))
	}
	if len(organizations) == 0 {
		return closeOnError(storagecatalog.ErrOrganization)
	}
	combined := sha256.Sum256([]byte("registry:" + hex.EncodeToString(registryCopy.digest[:]) + "\nauth:" + hex.EncodeToString(authCopy.digest[:])))
	copySource.source = ImportSource{
		RegistryDB:     copySource.registryDB,
		AuthDB:         copySource.authDB,
		Catalog:        catalog,
		Registry:       storagecatalog.Source{Schema: "registry_copy", Store: "registry", CatalogVersion: catalog.Version, Organizations: organizations},
		Auth:           storagecatalog.Source{Schema: "auth_copy", Store: "auth", CatalogVersion: catalog.Version, Organizations: organizations},
		RegistrySchema: registrySchema,
		AuthSchema:     authSchema,
		SnapshotID:     "sqlite-copy:" + hex.EncodeToString(combined[:]),
		copyVerifier:   sqliteCopyVerifier{copies: copySource.files},
	}
	if err := (sqliteCopyVerifier{copies: copySource.files}).Verify(ctx, false); err != nil {
		return closeOnError(err)
	}
	return copySource, nil
}

func (s *SQLiteCopySource) ImportSource() ImportSource {
	if s == nil {
		return ImportSource{}
	}
	return s.source
}

func (s *SQLiteCopySource) Close() error {
	if s == nil {
		return nil
	}
	var errs []error
	if s.registryDB != nil {
		errs = append(errs, s.registryDB.Close())
	}
	if s.authDB != nil {
		errs = append(errs, s.authDB.Close())
	}
	for _, copy := range s.files {
		if copy.file != nil {
			errs = append(errs, copy.file.Close())
		}
	}
	return errors.Join(errs...)
}

func openSealedSQLiteCopy(ctx context.Context, path string, applicationID int64) (*sql.DB, *sealedSQLiteCopy, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, ErrImport
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, nil, ErrImport
	}
	absolute = resolved
	info, err := os.Lstat(absolute)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o222 != 0 {
		return nil, nil, ErrImport
	}
	copy := &sealedSQLiteCopy{path: absolute, info: info}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(absolute + suffix); err == nil || !errors.Is(err, os.ErrNotExist) {
			return nil, nil, ErrImport
		}
	}
	file, err := os.Open(absolute)
	if err != nil {
		return nil, nil, ErrImport
	}
	copy.file = file
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) || openedInfo.Size() == 0 {
		return nil, nil, errors.Join(ErrImport, file.Close())
	}
	digest, err := hashSQLiteCopy(ctx, file, openedInfo.Size())
	if err != nil {
		return nil, nil, errors.Join(ErrImport, file.Close())
	}
	finalInfo, err := file.Stat()
	pathInfo, pathErr := os.Lstat(absolute)
	if err != nil || pathErr != nil || !os.SameFile(info, finalInfo) || !os.SameFile(info, pathInfo) || finalInfo.Size() != info.Size() || !finalInfo.ModTime().Equal(info.ModTime()) || pathInfo.Mode().Perm()&0o222 != 0 {
		return nil, nil, errors.Join(ErrImport, file.Close())
	}
	copy.digest = digest
	dsn := (&url.URL{Scheme: "file", Path: absolute}).String() + "?mode=ro&immutable=1"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, errors.Join(ErrImport, file.Close())
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		return nil, nil, errors.Join(ErrImport, db.Close(), file.Close())
	}
	db.SetConnMaxLifetime(0)
	var actualApplicationID int64
	if err := db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&actualApplicationID); err != nil || actualApplicationID != applicationID {
		return nil, nil, errors.Join(ErrImport, db.Close(), file.Close())
	}
	if _, err := db.ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
		return nil, nil, errors.Join(ErrImport, db.Close(), file.Close())
	}
	return db, copy, nil
}

func hashSQLiteCopy(ctx context.Context, file *os.File, size int64) ([32]byte, error) {
	hash := sha256.New()
	buffer := make([]byte, 64*1024)
	for offset := int64(0); offset < size; {
		if err := ctx.Err(); err != nil {
			return [32]byte{}, err
		}
		remaining := size - offset
		if remaining < int64(len(buffer)) {
			buffer = buffer[:remaining]
		}
		read, err := file.ReadAt(buffer, offset)
		if err != nil {
			return [32]byte{}, err
		}
		if read == 0 {
			return [32]byte{}, io.ErrUnexpectedEOF
		}
		if _, err := hash.Write(buffer[:read]); err != nil {
			return [32]byte{}, err
		}
		offset += int64(read)
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

func (s sqliteCopyVerifier) Verify(ctx context.Context, complete bool) error {
	for _, copy := range s.copies {
		if err := ctx.Err(); err != nil {
			return err
		}
		pathInfo, err := os.Lstat(copy.path)
		if err != nil || !pathInfo.Mode().IsRegular() || pathInfo.Mode()&os.ModeSymlink != 0 || pathInfo.Mode().Perm()&0o222 != 0 || !os.SameFile(copy.info, pathInfo) {
			return ErrImport
		}
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			if _, err := os.Lstat(copy.path + suffix); err == nil || !errors.Is(err, os.ErrNotExist) {
				return ErrImport
			}
		}
		current, err := copy.file.Stat()
		if err != nil || current.Size() != copy.info.Size() || !current.ModTime().Equal(copy.info.ModTime()) {
			return ErrImport
		}
		if complete {
			digest, err := hashSQLiteCopy(ctx, copy.file, copy.info.Size())
			if err != nil || !slices.Equal(digest[:], copy.digest[:]) {
				return ErrImport
			}
			finalInfo, err := copy.file.Stat()
			finalPathInfo, pathErr := os.Lstat(copy.path)
			if err != nil || pathErr != nil || !os.SameFile(copy.info, finalInfo) || !os.SameFile(copy.info, finalPathInfo) || finalInfo.Size() != current.Size() || !finalInfo.ModTime().Equal(current.ModTime()) || finalPathInfo.Mode().Perm()&0o222 != 0 {
				return ErrImport
			}
		}
	}
	return nil
}

func sqliteCatalogSchema(catalog storagecatalog.Catalog, store string) (storagecatalog.Schema, error) {
	schema := make(storagecatalog.Schema)
	for name, table := range catalog.Tables {
		tableName, ok := strings.CutPrefix(name, store+"/")
		if !ok {
			continue
		}
		columns := make(map[string]string, len(table.Columns))
		for column, definition := range table.Columns {
			columns[column] = definition.Source
		}
		schema[tableName] = columns
	}
	if err := catalog.ValidateSchema(store, schema); err != nil {
		return nil, err
	}
	return schema, nil
}

func validateSQLiteStore(ctx context.Context, db *sql.DB, catalog storagecatalog.Catalog, store string, schema storagecatalog.Schema) error {
	wantTables := make(map[string]struct{}, len(schema))
	for name := range schema {
		if name != "sqlite_schema" && name != "sqlite_sequence" {
			wantTables[name] = struct{}{}
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT name,type FROM sqlite_master WHERE type='table' AND name NOT GLOB 'sqlite_*' ORDER BY name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var litestreamTables []string
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			return err
		}
		if kind != "table" {
			return storagecatalog.ErrUnmapped
		}
		if _, ok := wantTables[name]; !ok {
			if optionalLitestreamTable(name) {
				litestreamTables = append(litestreamTables, name)
				continue
			}
			return storagecatalog.ErrUnmapped
		}
		delete(wantTables, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, name := range litestreamTables {
		if err := validateLitestreamTable(ctx, db, name); err != nil {
			return fmt.Errorf("validate Litestream metadata table %s: %w", name, err)
		}
	}
	if len(wantTables) != 0 {
		return storagecatalog.ErrUnmapped
	}
	for name := range schema {
		if name == "sqlite_schema" || name == "sqlite_sequence" {
			continue
		}
		if err := validateSQLiteTable(ctx, db, catalog.Tables[store+"/"+name], name); err != nil {
			return fmt.Errorf("validate SQLite table %s: %w", name, err)
		}
	}
	return nil
}

func optionalLitestreamTable(name string) bool {
	return name == "_litestream_lock" || name == "_litestream_seq"
}

func validateLitestreamTable(ctx context.Context, db *sql.DB, name string) error {
	columns := map[string]storagecatalog.Column{
		"id": {Source: `[0,"id","INTEGER",0,null,0,0]`},
	}
	if name == "_litestream_seq" {
		columns["id"] = storagecatalog.Column{Source: `[0,"id","INTEGER",0,null,1,0]`}
		columns["seq"] = storagecatalog.Column{Source: `[1,"seq","INTEGER",0,null,0,0]`}
	}
	return validateSQLiteTable(ctx, db, storagecatalog.Table{Columns: columns}, name)
}

type sqliteColumnDefinition struct {
	Ordinal int
	Name    string
	Type    string
	NotNull int
	Default sql.NullString
	Primary int
	Hidden  int
}

func validateSQLiteTable(ctx context.Context, db *sql.DB, table storagecatalog.Table, name string) error {
	rows, err := db.QueryContext(ctx, `SELECT cid,name,type,"notnull",dflt_value,pk,hidden FROM pragma_table_xinfo(?)`, name)
	if err != nil {
		return err
	}
	defer rows.Close()
	columns := make(map[string]sqliteColumnDefinition, len(table.Columns))
	for rows.Next() {
		var column sqliteColumnDefinition
		if err := rows.Scan(&column.Ordinal, &column.Name, &column.Type, &column.NotNull, &column.Default, &column.Primary, &column.Hidden); err != nil {
			return err
		}
		columns[column.Name] = column
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(columns) != len(table.Columns) {
		return storagecatalog.ErrUnmapped
	}
	for name, expected := range table.Columns {
		actual, ok := columns[name]
		if !ok {
			return storagecatalog.ErrUnmapped
		}
		var expectedFields []json.RawMessage
		if json.Unmarshal([]byte(expected.Source), &expectedFields) != nil || len(expectedFields) != 7 {
			return storagecatalog.ErrUnmapped
		}
		defaultValue := any(nil)
		if actual.Default.Valid {
			defaultValue = actual.Default.String
		}
		actualFields, err := json.Marshal([]any{actual.Ordinal, actual.Name, actual.Type, actual.NotNull, defaultValue, actual.Primary, actual.Hidden})
		if err != nil {
			return err
		}
		var expectedValue, actualValue any
		if json.Unmarshal(expectedFieldsBytes(expectedFields), &expectedValue) != nil || json.Unmarshal(actualFields, &actualValue) != nil {
			return storagecatalog.ErrUnmapped
		}
		expectedDefinition, expectedOK := expectedValue.([]any)
		actualDefinition, actualOK := actualValue.([]any)
		if expectedOK && actualOK && len(expectedDefinition) == 7 && len(actualDefinition) == 7 {
			expectedValue = expectedDefinition[1:]
			actualValue = actualDefinition[1:]
		}
		if !sameJSONValue(expectedValue, actualValue) {
			return fmt.Errorf("%w: source SQLite definition differs for column %s: catalog=%s actual=%s", storagecatalog.ErrUnmapped, name, expectedFieldsBytes(expectedFields), actualFields)
		}
	}
	return rows.Close()
}

func expectedFieldsBytes(fields []json.RawMessage) []byte {
	return []byte("[" + strings.Join(func() []string {
		values := make([]string, len(fields))
		for n, field := range fields {
			values[n] = string(field)
		}
		return values
	}(), ",") + "]")
}

func sameJSONValue(left, right any) bool {
	leftData, leftErr := json.Marshal(left)
	rightData, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftData) == string(rightData)
}

func sqliteOrganizations(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM organizations ORDER BY id COLLATE BINARY`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var organizations []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		organizations = append(organizations, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return organizations, rows.Close()
}
