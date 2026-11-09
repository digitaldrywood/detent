package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

const continuationIndexPageRows = neutralImportPageRows
const continuationIndexFanIn = 4

type conversationMessageIndex struct {
	directory string
	view      *VerifiedNeutralImport
	rows      int64
}

func buildConversationMessageIndex(ctx context.Context, source *VerifiedNeutralImport) (index *conversationMessageIndex, result error) {
	table, err := source.table("conversation_messages")
	if err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp("", "conversation-message-index-")
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(directory))
	}
	index = &conversationMessageIndex{directory: directory, view: &VerifiedNeutralImport{root: root, pageOffsets: make(map[string]int64)}}
	defer func() {
		if result != nil {
			result = errors.Join(result, index.close())
			index = nil
		}
	}()
	projection := NeutralImportTable{Name: "conversation_messages_index", Columns: make(map[string]string), Key: []NeutralImportKey{{Column: "conversation_id", Kind: "text"}, {Column: "seq", Kind: "integer"}, {Column: "id", Kind: "text"}}}
	for _, name := range []string{"id", "conversation_id", "seq", "role", "kind", "command_key"} {
		projection.Columns[name] = table.Columns[name]
	}
	var after []string
	var sourceRows int64
	runs := 0
	for {
		page, err := source.Page(ctx, table.Name, after, continuationIndexPageRows)
		if err != nil {
			return index, err
		}
		if len(page) == 0 {
			break
		}
		after = page[len(page)-1].Key
		for position, row := range page {
			page[position], err = continuationMessageProjection(row)
			if err != nil {
				return index, err
			}
			sourceRows++
		}
		slices.SortFunc(page, func(a, b NeutralImportRow) int { return compareNeutralImportKeys(projection.Key, a.Key, b.Key) })
		if err := writeContinuationRun(root, continuationRunName(0, runs), page); err != nil {
			return index, err
		}
		runs++
	}
	generation := 0
	for runs > 1 {
		next := 0
		for start := 0; start < runs; start += continuationIndexFanIn {
			var inputs []string
			for position := start; position < min(start+continuationIndexFanIn, runs); position++ {
				inputs = append(inputs, continuationRunName(generation, position))
			}
			if err := mergeContinuationRuns(ctx, root, projection.Key, inputs, continuationRunName(generation+1, next)); err != nil {
				return index, err
			}
			for _, name := range inputs {
				if err := root.Remove(name); err != nil {
					return index, err
				}
			}
			next++
		}
		generation, runs = generation+1, next
	}
	if runs == 1 {
		if err := index.publish(ctx, &projection, continuationRunName(generation, 0)); err != nil {
			return index, err
		}
		if err := root.Remove(continuationRunName(generation, 0)); err != nil {
			return index, err
		}
	}
	index.view.manifest = NeutralImportManifest{Version: 1, Identity: source.Identity(), Tables: []NeutralImportTable{projection}}
	if index.rows != sourceRows {
		return index, ErrNeutralImport
	}
	after = nil
	for {
		page, err := source.Page(ctx, table.Name, after, continuationIndexPageRows)
		if err != nil {
			return index, err
		}
		if len(page) == 0 {
			break
		}
		after = page[len(page)-1].Key
		for _, row := range page {
			projected, err := continuationMessageProjection(row)
			if err != nil {
				return index, err
			}
			actual, err := index.view.Lookup(ctx, projection.Name, projected.Key)
			if err != nil {
				return index, err
			}
			for name, expected := range projected.Values {
				value := actual.Values[name]
				if value.Null != expected.Null || value.Encoding != expected.Encoding || !bytes.Equal(value.Data, expected.Data) {
					return index, ErrNeutralImport
				}
			}
		}
	}
	return index, nil
}

func continuationMessageProjection(row NeutralImportRow) (NeutralImportRow, error) {
	values := make(map[string]storagecatalog.Cell)
	for _, name := range []string{"id", "conversation_id", "seq", "role", "kind", "command_key"} {
		value, found := row.Values[name]
		if !found {
			return NeutralImportRow{}, ErrNeutralImport
		}
		values[name] = value
	}
	sequence, err := strconv.ParseInt(cellText(values["seq"]), 10, 64)
	if err != nil || sequence <= 0 || cellText(values["id"]) == "" || cellText(values["conversation_id"]) == "" {
		return NeutralImportRow{}, ErrNeutralImport
	}
	return NeutralImportRow{SourceRowID: row.SourceRowID, Key: []string{cellText(values["conversation_id"]), strconv.FormatInt(sequence, 10), cellText(values["id"])}, Values: values}, nil
}

func continuationRunName(generation, index int) string {
	return fmt.Sprintf("run_%d_%d.jsonl", generation, index)
}

func writeContinuationRun(root *os.Root, name string, rows []NeutralImportRow) (result error) {
	file, err := root.Create(name)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	for _, row := range rows {
		if err := json.NewEncoder(file).Encode(row); err != nil {
			return err
		}
	}
	return nil
}

func mergeContinuationRuns(ctx context.Context, root *os.Root, keys []NeutralImportKey, inputs []string, output string) (result error) {
	type reader struct {
		decoder *json.Decoder
		row     *NeutralImportRow
	}
	var readers []reader
	for _, name := range inputs {
		file, err := root.Open(name)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, file.Close()) }()
		readers = append(readers, reader{decoder: json.NewDecoder(file)})
	}
	file, err := root.Create(output)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	advance := func(position int) error {
		var row NeutralImportRow
		err := readers[position].decoder.Decode(&row)
		readers[position].row = nil
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err == nil {
			if !validNeutralImportKey(keys, row.Key) {
				return ErrNeutralImport
			}
			readers[position].row = &row
		}
		return err
	}
	for position := range readers {
		if err := advance(position); err != nil {
			return err
		}
	}
	var previous []string
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		selected := -1
		for position, r := range readers {
			if r.row != nil && (selected < 0 || compareNeutralImportKeys(keys, r.row.Key, readers[selected].row.Key) < 0) {
				selected = position
			}
		}
		if selected < 0 {
			return nil
		}
		row := readers[selected].row
		if previous != nil && compareNeutralImportKeys(keys, previous, row.Key) >= 0 {
			return ErrNeutralImport
		}
		previous = row.Key
		if err := json.NewEncoder(file).Encode(row); err != nil {
			return err
		}
		if err := advance(selected); err != nil {
			return err
		}
	}
}

func (index *conversationMessageIndex) publish(ctx context.Context, table *NeutralImportTable, run string) (result error) {
	file, err := index.view.root.Open(run)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	metadata, err := json.Marshal(table)
	if err != nil {
		return err
	}
	metadataBytes := len(metadata) + 16
	var rows []NeutralImportRow
	pageBytes := 2
	flush := func() error {
		if len(rows) == 0 {
			return nil
		}
		raw, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		if len(raw) > neutralImportPageBytes {
			return ErrNeutralImport
		}
		name := fmt.Sprintf("messages_%d.json", len(table.Pages))
		if err := index.view.root.WriteFile(name, raw, 0400); err != nil {
			return err
		}
		page := NeutralImportPage{File: name, SHA256: artifact.Digest(raw), Rows: len(rows), First: rows[0].Key, Last: rows[len(rows)-1].Key}
		encoded, err := json.Marshal(page)
		if err != nil {
			return err
		}
		metadataBytes += len(encoded) + 1
		if metadataBytes > neutralImportManifestBytes {
			return ErrNeutralImport
		}
		index.view.pageOffsets[name] = index.rows
		index.rows += int64(len(rows))
		table.Pages = append(table.Pages, page)
		rows = nil
		pageBytes = 2
		return nil
	}
	decoder := json.NewDecoder(file)
	var previous []string
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var row NeutralImportRow
		if err := decoder.Decode(&row); errors.Is(err, io.EOF) {
			return flush()
		} else if err != nil {
			return err
		}
		if previous != nil && compareNeutralImportKeys(table.Key, previous, row.Key) >= 0 {
			return ErrNeutralImport
		}
		previous = row.Key
		encoded, err := json.Marshal(row)
		if err != nil {
			return err
		}
		if len(encoded)+2 > neutralImportPageBytes {
			return ErrNeutralImport
		}
		if pageBytes+len(encoded)+1 > neutralImportPageBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		pageBytes += len(encoded) + 1
		rows = append(rows, row)
		if len(rows) == continuationIndexPageRows {
			if err := flush(); err != nil {
				return err
			}
		}
	}
}

func (index *conversationMessageIndex) page(ctx context.Context, conversation string, after []string) ([]NeutralImportRow, error) {
	if len(after) == 0 {
		after = []string{conversation, "0", ""}
	}
	rows, err := index.view.Page(ctx, "conversation_messages_index", after, continuationIndexPageRows)
	if err != nil {
		return nil, err
	}
	for position, row := range rows {
		if row.Key[0] != conversation {
			return rows[:position], nil
		}
	}
	return rows, nil
}

func (index *conversationMessageIndex) close() error {
	return errors.Join(index.view.root.Close(), os.RemoveAll(index.directory))
}
