package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrNeutralImport = errors.New("invalid neutral import bundle")

const neutralImportPageRows = 100
const neutralImportPageBytes = 32 << 20
const neutralImportManifestBytes = 16 << 20

type NeutralImportIdentity struct {
	SourceID       string                 `json:"source_id"`
	SourceSHA256   string                 `json:"source_sha256"`
	OrganizationID tracker.OrganizationID `json:"organization_id"`
	SchemaVersion  int                    `json:"schema_version"`
}
type NeutralImportManifest struct {
	Version  int                   `json:"version"`
	Identity NeutralImportIdentity `json:"identity"`
	Tables   []NeutralImportTable  `json:"tables"`
}
type NeutralImportKey struct {
	Column string `json:"column"`
	Kind   string `json:"kind"`
}
type NeutralImportTable struct {
	Name    string              `json:"name"`
	Columns map[string]string   `json:"columns"`
	Key     []NeutralImportKey  `json:"key"`
	Pages   []NeutralImportPage `json:"pages"`
}
type NeutralImportPage struct {
	File   string   `json:"file"`
	SHA256 string   `json:"sha256"`
	Rows   int      `json:"rows"`
	First  []string `json:"first"`
	Last   []string `json:"last"`
}
type NeutralImportRow struct {
	SourceRowID int64                          `json:"source_row_id,omitempty"`
	Ordinal     int64                          `json:"-"`
	Key         []string                       `json:"key"`
	Values      map[string]storagecatalog.Cell `json:"values"`
}
type VerifiedNeutralImport struct {
	pageOffsets map[string]int64
	root        *os.Root
	manifest    NeutralImportManifest
	digest      string
}

func ReadNeutralImportBundle(ctx context.Context, directory string, organization tracker.OrganizationID, read func(*VerifiedNeutralImport) error) (resultErr error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return ErrNeutralImport
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	raw, err := readNeutralImportFile(ctx, root, "manifest.json", neutralImportManifestBytes)
	if err != nil {
		return err
	}
	var manifest NeutralImportManifest
	if err := decodeNeutralImport(raw, &manifest); err != nil {
		return err
	}
	source := &VerifiedNeutralImport{root: root, manifest: manifest, digest: artifact.Digest(raw)}
	if err := source.verify(ctx, organization); err != nil {
		return err
	}
	if read != nil {
		if err := read(source); err != nil {
			return err
		}
	}
	current, err := os.OpenRoot(directory)
	if err != nil {
		return mutation.ErrConflict
	}
	defer func() { resultErr = errors.Join(resultErr, current.Close()) }()
	final, err := readNeutralImportFile(ctx, current, "manifest.json", neutralImportManifestBytes)
	if err != nil || artifact.Digest(final) != source.digest {
		return mutation.ErrConflict
	}
	source.root = current
	if err := source.verify(ctx, organization); err != nil {
		return mutation.ErrConflict
	}
	return nil
}

func (s *VerifiedNeutralImport) Identity() NeutralImportIdentity { return s.manifest.Identity }
func (s *VerifiedNeutralImport) Digest() string                  { return s.digest }
func (s *VerifiedNeutralImport) Schema() storagecatalog.Schema {
	schema := storagecatalog.Schema{}
	for _, table := range s.manifest.Tables {
		columns := make(map[string]string, len(table.Columns))
		for name, signature := range table.Columns {
			columns[name] = signature
		}
		schema[table.Name] = columns
	}
	return schema
}
func (s *VerifiedNeutralImport) TableKey(name string) ([]NeutralImportKey, error) {
	table, err := s.table(name)
	if err != nil {
		return nil, err
	}
	return slices.Clone(table.Key), nil
}
func (s *VerifiedNeutralImport) table(name string) (NeutralImportTable, error) {
	for _, table := range s.manifest.Tables {
		if table.Name == name {
			return table, nil
		}
	}
	return NeutralImportTable{}, ErrNeutralImport
}
func (s *VerifiedNeutralImport) Page(ctx context.Context, name string, after []string, limit int) ([]NeutralImportRow, error) {
	if limit < 1 || limit > neutralImportPageRows {
		return nil, ErrNeutralImport
	}
	table, err := s.table(name)
	if err != nil {
		return nil, err
	}
	if len(after) != 0 && !validNeutralImportKey(table.Key, after) {
		return nil, ErrNeutralImport
	}
	start := sort.Search(len(table.Pages), func(i int) bool {
		return len(after) == 0 || compareNeutralImportKeys(table.Key, table.Pages[i].Last, after) > 0
	})
	if start == len(table.Pages) {
		return []NeutralImportRow{}, nil
	}
	rows, err := s.page(ctx, table, table.Pages[start])
	if err != nil {
		return nil, err
	}
	first := sort.Search(len(rows), func(i int) bool {
		return len(after) == 0 || compareNeutralImportKeys(table.Key, rows[i].Key, after) > 0
	})
	return rows[first:min(len(rows), first+limit)], nil
}
func (s *VerifiedNeutralImport) OrdinalPage(ctx context.Context, name string, after int64, limit int) ([]NeutralImportRow, error) {
	if after < 0 || limit < 1 || limit > neutralImportPageRows {
		return nil, ErrNeutralImport
	}
	table, err := s.table(name)
	if err != nil {
		return nil, err
	}
	page := sort.Search(len(table.Pages), func(i int) bool { return s.pageOffsets[table.Pages[i].File]+int64(table.Pages[i].Rows) > after })
	if page == len(table.Pages) {
		return []NeutralImportRow{}, nil
	}
	rows, err := s.page(ctx, table, table.Pages[page])
	if err != nil {
		return nil, err
	}
	first := int(after - s.pageOffsets[table.Pages[page].File])
	return rows[first:min(len(rows), first+limit)], nil
}

func (s *VerifiedNeutralImport) Lookup(ctx context.Context, name string, key []string) (NeutralImportRow, error) {
	table, err := s.table(name)
	if err != nil || !validNeutralImportKey(table.Key, key) {
		return NeutralImportRow{}, ErrNeutralImport
	}
	page := sort.Search(len(table.Pages), func(i int) bool { return compareNeutralImportKeys(table.Key, table.Pages[i].Last, key) >= 0 })
	if page == len(table.Pages) {
		return NeutralImportRow{}, corestore.ErrNotFound
	}
	rows, err := s.page(ctx, table, table.Pages[page])
	if err != nil {
		return NeutralImportRow{}, err
	}
	index := sort.Search(len(rows), func(i int) bool { return compareNeutralImportKeys(table.Key, rows[i].Key, key) >= 0 })
	if index == len(rows) || compareNeutralImportKeys(table.Key, rows[index].Key, key) != 0 {
		return NeutralImportRow{}, corestore.ErrNotFound
	}
	return rows[index], nil
}

func (s *VerifiedNeutralImport) verify(ctx context.Context, organization tracker.OrganizationID) error {
	identity := s.manifest.Identity
	if s.manifest.Version != 1 || organization == "" || identity.OrganizationID != organization || identity.SourceID == "" || strings.TrimSpace(identity.SourceID) != identity.SourceID || len(identity.SourceID) > 200 || !artifact.ValidHash(identity.SourceSHA256, 64) || identity.SchemaVersion != 1 || len(s.manifest.Tables) == 0 {
		return ErrNeutralImport
	}
	s.pageOffsets = make(map[string]int64)
	names := make(map[string]bool)
	files := map[string]bool{"manifest.json": true}
	for _, table := range s.manifest.Tables {
		if !neutralImportIdentifier(table.Name) || names[table.Name] || len(table.Columns) == 0 || len(table.Key) == 0 {
			return ErrNeutralImport
		}
		names[table.Name] = true
		keyNames := make(map[string]bool)
		for _, key := range table.Key {
			if !neutralImportIdentifier(key.Column) || table.Columns[key.Column] == "" || keyNames[key.Column] || key.Kind != "text" && key.Kind != "integer" {
				return ErrNeutralImport
			}
			keyNames[key.Column] = true
		}
		for name, signature := range table.Columns {
			if !neutralImportIdentifier(name) || !validNeutralImportColumn(name, signature) {
				return ErrNeutralImport
			}
		}
		var previous []string
		var ordinal int64
		sourceIDs := map[int64]bool{}
		for _, page := range table.Pages {
			if page.File == "" || filepath.Base(page.File) != page.File || page.File == "." || files[page.File] || !artifact.ValidHash(page.SHA256, 64) || page.Rows < 1 || page.Rows > neutralImportPageRows || !validNeutralImportKey(table.Key, page.First) || !validNeutralImportKey(table.Key, page.Last) || compareNeutralImportKeys(table.Key, page.First, page.Last) > 0 || previous != nil && compareNeutralImportKeys(table.Key, previous, page.First) >= 0 {
				return ErrNeutralImport
			}
			files[page.File] = true
			s.pageOffsets[page.File] = ordinal
			ordinal += int64(page.Rows)
			rows, err := s.page(ctx, table, page)
			if err != nil {
				return err
			}
			for _, row := range rows {
				if row.SourceRowID > 0 {
					if sourceIDs[row.SourceRowID] {
						return ErrNeutralImport
					}
					sourceIDs[row.SourceRowID] = true
				}
			}
			previous = page.Last
		}
	}
	return nil
}
func (s *VerifiedNeutralImport) page(ctx context.Context, table NeutralImportTable, page NeutralImportPage) ([]NeutralImportRow, error) {
	raw, err := readNeutralImportFile(ctx, s.root, page.File, neutralImportPageBytes)
	if err != nil {
		return nil, err
	}
	if artifact.Digest(raw) != page.SHA256 {
		return nil, mutation.ErrConflict
	}
	var rows []NeutralImportRow
	if err := decodeNeutralImport(raw, &rows); err != nil {
		return nil, err
	}
	if len(rows) != page.Rows {
		return nil, ErrNeutralImport
	}
	var previous []string
	for _, row := range rows {
		if row.SourceRowID < 0 {
			return nil, ErrNeutralImport
		}
		if row.SourceRowID > 0 && len(table.Key) == 1 && table.Key[0].Column == "id" && table.Key[0].Kind == "integer" {
			id, err := strconv.ParseInt(string(row.Values["id"].Data), 10, 64)
			if err != nil || id != row.SourceRowID {
				return nil, ErrNeutralImport
			}
		}
		if !validNeutralImportKey(table.Key, row.Key) || len(row.Values) != len(table.Columns) || previous != nil && compareNeutralImportKeys(table.Key, previous, row.Key) >= 0 {
			return nil, ErrNeutralImport
		}
		for name, cell := range row.Values {
			if _, ok := table.Columns[name]; !ok {
				return nil, ErrNeutralImport
			}
			if cell.Encoding == "utf8" && !utf8.Valid(cell.Data) {
				return nil, ErrNeutralImport
			}
			if cell.Null && (cell.Encoding != "" || len(cell.Data) != 0) || !cell.Null && cell.Encoding != "utf8" && cell.Encoding != "base64" && cell.Encoding != "hex" {
				return nil, ErrNeutralImport
			}
		}
		for index, key := range table.Key {
			value := row.Values[key.Column]
			if value.Null || value.Encoding != "utf8" || string(value.Data) != row.Key[index] {
				return nil, ErrNeutralImport
			}
		}
		if organization, ok := row.Values["organization_id"]; ok && (organization.Null || organization.Encoding != "utf8" || string(organization.Data) != string(s.manifest.Identity.OrganizationID)) {
			return nil, storagecatalog.ErrOrganization
		}
		previous = row.Key
	}
	for index := range rows {
		rows[index].Ordinal = s.pageOffsets[page.File] + int64(index) + 1
	}
	if !slices.Equal(rows[0].Key, page.First) || !slices.Equal(rows[len(rows)-1].Key, page.Last) {
		return nil, ErrNeutralImport
	}
	return rows, nil
}
func validNeutralImportColumn(name, signature string) bool {
	var fields []json.RawMessage
	if json.Unmarshal([]byte(signature), &fields) != nil || len(fields) != 7 {
		return false
	}
	var ordinal, required, primary, hidden int
	var column, kind string
	var fallback *string
	if json.Unmarshal(fields[0], &ordinal) != nil || json.Unmarshal(fields[1], &column) != nil || json.Unmarshal(fields[2], &kind) != nil || json.Unmarshal(fields[3], &required) != nil || json.Unmarshal(fields[4], &fallback) != nil || json.Unmarshal(fields[5], &primary) != nil || json.Unmarshal(fields[6], &hidden) != nil {
		return false
	}
	return ordinal >= 0 && column == name && kind != "" && (required == 0 || required == 1) && primary >= 0 && hidden == 0
}

func neutralImportIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for index, ch := range value {
		if ch != '_' && (ch < 'a' || ch > 'z') && (index == 0 || ch < '0' || ch > '9') {
			return false
		}
	}
	return true
}
func validNeutralImportKey(columns []NeutralImportKey, values []string) bool {
	if len(columns) != len(values) {
		return false
	}
	for index, column := range columns {
		if column.Kind == "integer" {
			number, err := strconv.ParseInt(values[index], 10, 64)
			if err != nil || strconv.FormatInt(number, 10) != values[index] {
				return false
			}
		}
	}
	return true
}
func compareNeutralImportKeys(columns []NeutralImportKey, left, right []string) int {
	for index, column := range columns {
		if column.Kind == "integer" {
			a, _ := strconv.ParseInt(left[index], 10, 64)
			b, _ := strconv.ParseInt(right[index], 10, 64)
			if a < b {
				return -1
			}
			if a > b {
				return 1
			}
		} else {
			if left[index] < right[index] {
				return -1
			}
			if left[index] > right[index] {
				return 1
			}
		}
	}
	return 0
}
func readNeutralImportFile(ctx context.Context, root *os.Root, name string, limit int64) (raw []byte, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, ErrNeutralImport
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0222 != 0 || before.Size() > limit {
		return nil, ErrNeutralImport
	}
	raw, err = io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, ErrNeutralImport
	}
	after, err := file.Stat()
	if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, mutation.ErrConflict
	}
	return raw, ctx.Err()
}
func decodeNeutralImport(raw []byte, destination any) error {
	if !utf8.Valid(raw) {
		return ErrNeutralImport
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return ErrNeutralImport
	}
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return ErrNeutralImport
	}
	return nil
}

func (r NeutralImportRow) Decode(fields map[string]any) error {
	for name, destination := range fields {
		cell, ok := r.Values[name]
		if !ok || !cell.Null && cell.Encoding != "utf8" {
			return fmt.Errorf("decode neutral field %s: %w", name, ErrNeutralImport)
		}
		if scanner, ok := destination.(interface{ Scan(any) error }); ok {
			var value any
			if !cell.Null {
				value = string(cell.Data)
			}
			if err := scanner.Scan(value); err != nil {
				return fmt.Errorf("decode neutral field %s: %w", name, ErrNeutralImport)
			}
			continue
		}
		if destination == nil {
			return fmt.Errorf("decode neutral field %s: %w", name, ErrNeutralImport)
		}
		value := reflect.ValueOf(destination)
		if value.Kind() != reflect.Pointer || value.IsNil() {
			return fmt.Errorf("decode neutral field %s: %w", name, ErrNeutralImport)
		}
		value = value.Elem()
		if cell.Null {
			if value.Kind() != reflect.Pointer {
				return fmt.Errorf("decode neutral field %s: %w", name, ErrNeutralImport)
			}
			value.SetZero()
			continue
		}
		for value.Kind() == reflect.Pointer {
			value.Set(reflect.New(value.Type().Elem()))
			value = value.Elem()
		}
		text := string(cell.Data)
		switch value.Kind() {
		case reflect.String:
			value.SetString(text)
		case reflect.Bool:
			if text != "0" && text != "1" {
				return fmt.Errorf("decode neutral field %s: %w", name, ErrNeutralImport)
			}
			value.SetBool(text == "1")
		case reflect.Int, reflect.Int32, reflect.Int64:
			number, err := strconv.ParseInt(text, 10, 64)
			if err != nil || value.OverflowInt(number) {
				return fmt.Errorf("decode neutral field %s: %w", name, ErrNeutralImport)
			}
			value.SetInt(number)
		case reflect.Float32, reflect.Float64:
			number, err := strconv.ParseFloat(text, 64)
			if err != nil || math.IsNaN(number) || math.IsInf(number, 0) || value.OverflowFloat(number) {
				return fmt.Errorf("decode neutral field %s: %w", name, ErrNeutralImport)
			}
			value.SetFloat(number)
		default:
			return fmt.Errorf("decode neutral field %s: %w", name, ErrNeutralImport)
		}
	}
	return nil
}
