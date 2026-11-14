package postgres

import (
	"encoding/json"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

func TestArchivedImportRowPreservesNullCells(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		raw  json.RawMessage
		null bool
		text string
	}{
		{name: "null", raw: json.RawMessage("null"), null: true},
		{name: "empty text", raw: json.RawMessage(`""`)},
		{name: "text", raw: json.RawMessage(`"example"`), text: "example"},
	} {
		t.Run(test.name, func(t *testing.T) {
			row, err := archivedImportRow(attachment.ArchiveTable{Columns: []string{"rowid", "value"}}, []json.RawMessage{json.RawMessage("1"), test.raw})
			if err != nil {
				t.Fatal(err)
			}
			catalog := storagecatalog.Catalog{Tables: map[string]storagecatalog.Table{"example": {Columns: map[string]storagecatalog.Column{"value": {Disposition: "column", Codec: "text", Null: "preserve", Empty: "preserve"}}}}}
			value, err := catalog.Decode("example", "value", row.Values["value"])
			if err != nil || value.Null != test.null || value.Text != test.text {
				t.Fatalf("decoded archived cell=%+v error=%v", value, err)
			}
		})
	}
}
