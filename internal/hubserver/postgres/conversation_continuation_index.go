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
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

const continuationIndexPageRows = neutralimport.PageRows
const continuationIndexFanIn = 4

type conversationMessageIndex struct {
	directory string
	root      *os.Root
	view      *neutralimport.Bundle
	rows      int64
}

func buildConversationMessageIndex(ctx context.Context, source *neutralimport.Bundle) (index *conversationMessageIndex, result error) {
	table, err := source.Table("conversation_messages")
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
	index = &conversationMessageIndex{directory: directory, root: root}
	defer func() {
		if result != nil {
			result = errors.Join(result, index.close())
			index = nil
		}
	}()
	projection := neutralimport.Table{Name: "conversation_messages_index", Columns: make(map[string]string), Key: []neutralimport.Key{{Column: "conversation_id", Kind: "text"}, {Column: "seq", Kind: "integer"}, {Column: "id", Kind: "text"}}}
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
		slices.SortFunc(page, func(a, b neutralimport.Row) int { return neutralimport.CompareKeys(projection.Key, a.Key, b.Key) })
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
	index.view = neutralimport.View(root, neutralimport.Manifest{Version: 1, Identity: source.Identity(), Tables: []neutralimport.Table{projection}})
	if index.rows != sourceRows {
		return index, neutralimport.ErrInvalid
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
					return index, neutralimport.ErrInvalid
				}
			}
		}
	}
	return index, nil
}

func continuationMessageProjection(row neutralimport.Row) (neutralimport.Row, error) {
	values := make(map[string]storagecatalog.Cell)
	for _, name := range []string{"id", "conversation_id", "seq", "role", "kind", "command_key"} {
		value, found := row.Values[name]
		if !found {
			return neutralimport.Row{}, neutralimport.ErrInvalid
		}
		values[name] = value
	}
	sequence, err := strconv.ParseInt(cellText(values["seq"]), 10, 64)
	if err != nil || sequence <= 0 || cellText(values["id"]) == "" || cellText(values["conversation_id"]) == "" {
		return neutralimport.Row{}, neutralimport.ErrInvalid
	}
	return neutralimport.Row{SourceRowID: row.SourceRowID, Key: []string{cellText(values["conversation_id"]), strconv.FormatInt(sequence, 10), cellText(values["id"])}, Values: values}, nil
}

func continuationRunName(generation, index int) string {
	return fmt.Sprintf("run_%d_%d.jsonl", generation, index)
}

func writeContinuationRun(root *os.Root, name string, rows []neutralimport.Row) (result error) {
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

func mergeContinuationRuns(ctx context.Context, root *os.Root, keys []neutralimport.Key, inputs []string, output string) (result error) {
	type reader struct {
		decoder *json.Decoder
		row     *neutralimport.Row
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
		var row neutralimport.Row
		err := readers[position].decoder.Decode(&row)
		readers[position].row = nil
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err == nil {
			if !neutralimport.ValidKey(keys, row.Key) {
				return neutralimport.ErrInvalid
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
			if r.row != nil && (selected < 0 || neutralimport.CompareKeys(keys, r.row.Key, readers[selected].row.Key) < 0) {
				selected = position
			}
		}
		if selected < 0 {
			return nil
		}
		row := readers[selected].row
		if previous != nil && neutralimport.CompareKeys(keys, previous, row.Key) >= 0 {
			return neutralimport.ErrInvalid
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

func (index *conversationMessageIndex) publish(ctx context.Context, table *neutralimport.Table, run string) (result error) {
	file, err := index.root.Open(run)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	metadata, err := json.Marshal(table)
	if err != nil {
		return err
	}
	metadataBytes := len(metadata) + 16
	var rows []neutralimport.Row
	pageBytes := 2
	flush := func() error {
		if len(rows) == 0 {
			return nil
		}
		raw, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		if len(raw) > neutralimport.PageBytes {
			return neutralimport.ErrInvalid
		}
		name := fmt.Sprintf("messages_%d.json", len(table.Pages))
		if err := index.root.WriteFile(name, raw, 0400); err != nil {
			return err
		}
		page := neutralimport.Page{File: name, SHA256: artifact.Digest(raw), Rows: len(rows), First: rows[0].Key, Last: rows[len(rows)-1].Key}
		encoded, err := json.Marshal(page)
		if err != nil {
			return err
		}
		metadataBytes += len(encoded) + 1
		if metadataBytes > neutralimport.ManifestBytes {
			return neutralimport.ErrInvalid
		}
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
		var row neutralimport.Row
		if err := decoder.Decode(&row); errors.Is(err, io.EOF) {
			return flush()
		} else if err != nil {
			return err
		}
		if previous != nil && neutralimport.CompareKeys(table.Key, previous, row.Key) >= 0 {
			return neutralimport.ErrInvalid
		}
		previous = row.Key
		encoded, err := json.Marshal(row)
		if err != nil {
			return err
		}
		if len(encoded)+2 > neutralimport.PageBytes {
			return neutralimport.ErrInvalid
		}
		if pageBytes+len(encoded)+1 > neutralimport.PageBytes {
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

func (index *conversationMessageIndex) page(ctx context.Context, conversation string, after []string) ([]neutralimport.Row, error) {
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
	return errors.Join(index.root.Close(), os.RemoveAll(index.directory))
}
