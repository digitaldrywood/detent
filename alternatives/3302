package neutralimport

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/hex"
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

var ErrInvalid = errors.New("invalid neutral import bundle")

const PageRows = 100
const PageBytes = 32 << 20
const ManifestBytes = 16 << 20

type Identity struct {
	SourceID       string                 `json:"source_id"`
	SourceSHA256   string                 `json:"source_sha256"`
	OrganizationID tracker.OrganizationID `json:"organization_id"`
	SchemaVersion  int                    `json:"schema_version"`
}
type Manifest struct {
	Version  int      `json:"version"`
	Identity Identity `json:"identity"`
	Tables   []Table  `json:"tables"`
}
type Key struct {
	Column string `json:"column"`
	Kind   string `json:"kind"`
}
type Table struct {
	Name    string            `json:"name"`
	Columns map[string]string `json:"columns"`
	Key     []Key             `json:"key"`
	Pages   []Page            `json:"pages"`
}
type Page struct {
	File   string   `json:"file"`
	SHA256 string   `json:"sha256"`
	Rows   int      `json:"rows"`
	First  []string `json:"first"`
	Last   []string `json:"last"`
}
type Row struct {
	SourcePosition *int64                         `json:"source_position,omitempty"`
	SourceRowID    int64                          `json:"source_row_id,omitempty"`
	Ordinal        int64                          `json:"-"`
	Key            []string                       `json:"key"`
	Values         map[string]storagecatalog.Cell `json:"values"`
}
type Bundle struct {
	pageOffsets map[string]int64
	root        *os.Root
	manifest    Manifest
	digest      string
}

func View(root *os.Root, manifest Manifest) *Bundle {
	offsets := make(map[string]int64)
	for _, table := range manifest.Tables {
		var ordinal int64
		for _, page := range table.Pages {
			offsets[page.File] = ordinal
			ordinal += int64(page.Rows)
		}
	}
	return &Bundle{root: root, manifest: manifest, pageOffsets: offsets}
}

func Read(ctx context.Context, directory string, organization tracker.OrganizationID, read func(*Bundle) error) (resultErr error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return ErrInvalid
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	raw, err := readNeutralImportFile(ctx, root, "manifest.json", ManifestBytes)
	if err != nil {
		return err
	}
	var manifest Manifest
	if err := decodeNeutralImport(raw, &manifest); err != nil {
		return err
	}
	source := &Bundle{root: root, manifest: manifest, digest: artifact.Digest(raw)}
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
	final, err := readNeutralImportFile(ctx, current, "manifest.json", ManifestBytes)
	if err != nil || artifact.Digest(final) != source.digest {
		return mutation.ErrConflict
	}
	source.root = current
	if err := source.verify(ctx, organization); err != nil {
		return mutation.ErrConflict
	}
	return nil
}

func (s *Bundle) Identity() Identity { return s.manifest.Identity }
func (s *Bundle) Digest() string     { return s.digest }
func (s *Bundle) Manifest() Manifest { return s.manifest }
func (s *Bundle) PageFile(ctx context.Context, name string) ([]byte, error) {
	return readNeutralImportFile(ctx, s.root, name, PageBytes)
}
func (s *Bundle) Schema() storagecatalog.Schema {
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
func (s *Bundle) TableKey(name string) ([]Key, error) {
	table, err := s.Table(name)
	if err != nil {
		return nil, err
	}
	return slices.Clone(table.Key), nil
}
func (s *Bundle) Table(name string) (Table, error) {
	for _, table := range s.manifest.Tables {
		if table.Name == name {
			return table, nil
		}
	}
	return Table{}, ErrInvalid
}
func (s *Bundle) Page(ctx context.Context, name string, after []string, limit int) ([]Row, error) {
	if limit < 1 || limit > PageRows {
		return nil, ErrInvalid
	}
	table, err := s.Table(name)
	if err != nil {
		return nil, err
	}
	if len(after) != 0 && !ValidKey(table.Key, after) {
		return nil, ErrInvalid
	}
	start := sort.Search(len(table.Pages), func(i int) bool {
		return len(after) == 0 || CompareKeys(table.Key, table.Pages[i].Last, after) > 0
	})
	if start == len(table.Pages) {
		return []Row{}, nil
	}
	rows, err := s.page(ctx, table, table.Pages[start])
	if err != nil {
		return nil, err
	}
	first := sort.Search(len(rows), func(i int) bool {
		return len(after) == 0 || CompareKeys(table.Key, rows[i].Key, after) > 0
	})
	return rows[first:min(len(rows), first+limit)], nil
}
func (s *Bundle) OrdinalPage(ctx context.Context, name string, after int64, limit int) ([]Row, error) {
	if after < 0 || limit < 1 || limit > PageRows {
		return nil, ErrInvalid
	}
	table, err := s.Table(name)
	if err != nil {
		return nil, err
	}
	page := sort.Search(len(table.Pages), func(i int) bool { return s.pageOffsets[table.Pages[i].File]+int64(table.Pages[i].Rows) > after })
	if page == len(table.Pages) {
		return []Row{}, nil
	}
	rows, err := s.page(ctx, table, table.Pages[page])
	if err != nil {
		return nil, err
	}
	first := int(after - s.pageOffsets[table.Pages[page].File])
	return rows[first:min(len(rows), first+limit)], nil
}

func (s *Bundle) Lookup(ctx context.Context, name string, key []string) (Row, error) {
	table, err := s.Table(name)
	if err != nil || !ValidKey(table.Key, key) {
		return Row{}, ErrInvalid
	}
	page := sort.Search(len(table.Pages), func(i int) bool { return CompareKeys(table.Key, table.Pages[i].Last, key) >= 0 })
	if page == len(table.Pages) {
		return Row{}, corestore.ErrNotFound
	}
	rows, err := s.page(ctx, table, table.Pages[page])
	if err != nil {
		return Row{}, err
	}
	index := sort.Search(len(rows), func(i int) bool { return CompareKeys(table.Key, rows[i].Key, key) >= 0 })
	if index == len(rows) || CompareKeys(table.Key, rows[index].Key, key) != 0 {
		return Row{}, corestore.ErrNotFound
	}
	return rows[index], nil
}

func (s *Bundle) verify(ctx context.Context, organization tracker.OrganizationID) error {
	identity := s.manifest.Identity
	if s.manifest.Version != 1 || organization == "" || identity.OrganizationID != organization || identity.SourceID == "" || strings.TrimSpace(identity.SourceID) != identity.SourceID || len(identity.SourceID) > 200 || !artifact.ValidHash(identity.SourceSHA256, 64) || identity.SchemaVersion != 1 || len(s.manifest.Tables) == 0 {
		return ErrInvalid
	}
	s.pageOffsets = make(map[string]int64)
	names := make(map[string]bool)
	files := map[string]bool{"manifest.json": true}
	for _, table := range s.manifest.Tables {
		if !neutralImportIdentifier(table.Name) || names[table.Name] || len(table.Columns) == 0 || len(table.Key) == 0 {
			return ErrInvalid
		}
		names[table.Name] = true
		keyNames := make(map[string]bool)
		for _, key := range table.Key {
			if !neutralImportIdentifier(key.Column) || table.Columns[key.Column] == "" || keyNames[key.Column] || key.Kind != "text" && key.Kind != "integer" {
				return ErrInvalid
			}
			keyNames[key.Column] = true
		}
		for name, signature := range table.Columns {
			if !neutralImportIdentifier(name) || !validNeutralImportColumn(name, signature) {
				return ErrInvalid
			}
		}
		var previous []string
		var ordinal int64
		sourceIDs := map[int64]bool{}
		for _, page := range table.Pages {
			if page.File == "" || filepath.Base(page.File) != page.File || page.File == "." || files[page.File] || !artifact.ValidHash(page.SHA256, 64) || page.Rows < 1 || page.Rows > PageRows || !ValidKey(table.Key, page.First) || !ValidKey(table.Key, page.Last) || CompareKeys(table.Key, page.First, page.Last) > 0 || previous != nil && CompareKeys(table.Key, previous, page.First) >= 0 {
				return ErrInvalid
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
						return ErrInvalid
					}
					sourceIDs[row.SourceRowID] = true
				}
			}
			previous = page.Last
		}
	}
	return nil
}
func (s *Bundle) page(ctx context.Context, table Table, page Page) ([]Row, error) {
	raw, err := readNeutralImportFile(ctx, s.root, page.File, PageBytes)
	if err != nil {
		return nil, err
	}
	if artifact.Digest(raw) != page.SHA256 {
		return nil, mutation.ErrConflict
	}
	var rows []Row
	if err := decodeNeutralImport(raw, &rows); err != nil {
		return nil, err
	}
	if len(rows) != page.Rows {
		return nil, ErrInvalid
	}
	var previous []string
	for _, row := range rows {
		if row.SourceRowID < 0 || row.SourcePosition != nil && *row.SourcePosition < 0 {
			return nil, ErrInvalid
		}
		if row.SourceRowID > 0 && len(table.Key) == 1 && table.Key[0].Column == "id" && table.Key[0].Kind == "integer" {
			id, err := strconv.ParseInt(string(row.Values["id"].Data), 10, 64)
			if err != nil || id != row.SourceRowID {
				return nil, ErrInvalid
			}
		}
		if !ValidKey(table.Key, row.Key) || len(row.Values) != len(table.Columns) || previous != nil && CompareKeys(table.Key, previous, row.Key) >= 0 {
			return nil, ErrInvalid
		}
		for name, cell := range row.Values {
			if _, ok := table.Columns[name]; !ok {
				return nil, ErrInvalid
			}
			if cell.Encoding == "utf8" && !utf8.Valid(cell.Data) {
				return nil, ErrInvalid
			}
			if cell.Null && (cell.Encoding != "" || len(cell.Data) != 0) || !cell.Null && cell.Encoding != "utf8" && cell.Encoding != "base64" && cell.Encoding != "hex" && cell.Encoding != "binary" {
				return nil, ErrInvalid
			}
		}
		for index, key := range table.Key {
			value := row.Values[key.Column]
			if value.Null || value.Encoding != "utf8" || string(value.Data) != row.Key[index] {
				return nil, ErrInvalid
			}
		}
		if organization, ok := row.Values["organization_id"]; ok {
			if organization.Null || organization.Encoding != "utf8" || table.Name != "hosted_transactions" && string(organization.Data) != string(s.manifest.Identity.OrganizationID) {
				return nil, storagecatalog.ErrOrganization
			}
		}
		previous = row.Key
	}
	for index := range rows {
		rows[index].Ordinal = s.pageOffsets[page.File] + int64(index) + 1
	}
	if !slices.Equal(rows[0].Key, page.First) || !slices.Equal(rows[len(rows)-1].Key, page.Last) {
		return nil, ErrInvalid
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
	return ordinal >= 0 && column == name && kind != "" && (required == 0 || required == 1) && primary >= 0 && (hidden == 0 || hidden == 2 || hidden == 3)
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
func ValidKey(columns []Key, values []string) bool {
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
func CompareKeys(columns []Key, left, right []string) int {
	for index, column := range columns {
		if column.Kind == "integer" {
			a, b := left[index], right[index]
			leftNegative, rightNegative := strings.HasPrefix(a, "-"), strings.HasPrefix(b, "-")
			if leftNegative != rightNegative {
				if leftNegative {
					return -1
				}
				return 1
			}
			direction := 1
			if leftNegative {
				direction = -1
			}
			if order := cmp.Compare(len(a), len(b)); order != 0 {
				return direction * order
			}
			if order := strings.Compare(a, b); order != 0 {
				return direction * order
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
		return nil, ErrInvalid
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0222 != 0 || before.Size() > limit {
		return nil, ErrInvalid
	}
	raw, err = io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, ErrInvalid
	}
	after, err := file.Stat()
	if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, mutation.ErrConflict
	}
	return raw, ctx.Err()
}
func decodeNeutralImport(raw []byte, destination any) error {
	if !utf8.Valid(raw) {
		return ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return ErrInvalid
	}
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return ErrInvalid
	}
	return nil
}

func (r Row) Decode(fields map[string]any) error {
	for name, destination := range fields {
		cell, ok := r.Values[name]
		if !ok || !cell.Null && cell.Encoding != "utf8" {
			return fmt.Errorf("decode neutral field %s: %w", name, ErrInvalid)
		}
		if scanner, ok := destination.(interface{ Scan(any) error }); ok {
			var value any
			if !cell.Null {
				value = string(cell.Data)
			}
			if err := scanner.Scan(value); err != nil {
				return fmt.Errorf("decode neutral field %s: %w", name, ErrInvalid)
			}
			continue
		}
		if destination == nil {
			return fmt.Errorf("decode neutral field %s: %w", name, ErrInvalid)
		}
		value := reflect.ValueOf(destination)
		if value.Kind() != reflect.Pointer || value.IsNil() {
			return fmt.Errorf("decode neutral field %s: %w", name, ErrInvalid)
		}
		value = value.Elem()
		if cell.Null {
			if value.Kind() != reflect.Pointer {
				return fmt.Errorf("decode neutral field %s: %w", name, ErrInvalid)
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
				return fmt.Errorf("decode neutral field %s: %w", name, ErrInvalid)
			}
			value.SetBool(text == "1")
		case reflect.Int, reflect.Int32, reflect.Int64:
			number, err := strconv.ParseInt(text, 10, 64)
			if err != nil || value.OverflowInt(number) {
				return fmt.Errorf("decode neutral field %s: %w", name, ErrInvalid)
			}
			value.SetInt(number)
		case reflect.Float32, reflect.Float64:
			number, err := strconv.ParseFloat(text, 64)
			if err != nil || math.IsNaN(number) || math.IsInf(number, 0) || value.OverflowFloat(number) {
				return fmt.Errorf("decode neutral field %s: %w", name, ErrInvalid)
			}
			value.SetFloat(number)
		default:
			return fmt.Errorf("decode neutral field %s: %w", name, ErrInvalid)
		}
	}
	return nil
}

func (r Row) Bytes(column string) ([]byte, error) {
	cell, ok := r.Values[column]
	if !ok || cell.Null {
		return nil, ErrInvalid
	}
	switch cell.Encoding {
	case "binary", "utf8":
		return bytes.Clone(cell.Data), nil
	case "base64":
		raw, err := base64.StdEncoding.DecodeString(string(cell.Data))
		if err != nil {
			return nil, ErrInvalid
		}
		return raw, nil
	case "hex":
		raw, err := hex.DecodeString(string(cell.Data))
		if err != nil {
			return nil, ErrInvalid
		}
		return raw, nil
	default:
		return nil, ErrInvalid
	}
}
