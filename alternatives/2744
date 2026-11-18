package boardviewsql

import (
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
)

func TestExpansionRestoresOneRowPerElement(t *testing.T) {
	t.Parallel()
	text := func(values ...string) []*string {
		out := make([]*string, len(values))
		for i := range values {
			if values[i] != "<nil>" {
				out[i] = &values[i]
			}
		}
		return out
	}
	number := func(values ...int64) []*int64 {
		out := make([]*int64, len(values))
		for i := range values {
			out[i] = &values[i]
		}
		return out
	}
	flag := func(values ...bool) []*bool {
		out := make([]*bool, len(values))
		for i := range values {
			out[i] = &values[i]
		}
		return out
	}
	closed := time.Date(2026, 11, 17, 6, 0, 0, 0, time.UTC)
	links := arrayRow{
		text:  [8][]*string{text("blocker", "related"), text("prj_a", "prj_b"), text("wi_card", "wi_card"), text("wi_one", "wi_two"), text("One", "Two"), text("Done", "Todo"), text("prj_a#1", "prj_b#2"), text("https://example.test/1", "https://example.test/2")},
		ints:  [2][]*int64{number(1, 2), number(1, 1)},
		bools: [3][]*bool{flag(true, false), flag(false, true), nil},
		times: [2][]*time.Time{{&closed, nil}, {nil, nil}},
	}
	states := arrayRow{text: [8][]*string{text("prj_a", "prj_a"), text("Todo", "Done"), text("Done", "<nil>")}, ints: [2][]*int64{number(1, 2), number(1, 0)}, bools: [3][]*bool{flag(false, true), flag(true, false), flag(false, false)}}
	short := links
	short.ints[1] = number(1)
	for _, test := range []struct {
		name  string
		kind  string
		input arrayRow
		check func(*testing.T, []viewstore.BoardViewRow)
		err   error
	}{
		{name: "links", kind: "links", input: links, check: func(t *testing.T, rows []viewstore.BoardViewRow) {
			if len(rows) != 2 {
				t.Fatalf("rows = %d, want 2", len(rows))
			}
			first := rows[0]
			if first.Kind != "link" || *first.Project != "prj_a" || *first.ID != "wi_card" || *first.Text[0] != "blocker" || *first.Text[1] != "wi_one" || *first.Text[4] != "prj_a#1" ||
				*first.Text[5] != "https://example.test/1" || *first.Int[0] != 1 || !*first.Bool[0] || *first.Bool[1] || !*first.Bool[2] || !first.Time[0].Equal(closed) || first.Time[1] != nil {
				t.Fatalf("first link = %+v", first)
			}
			if *rows[1].Text[0] != "related" || rows[1].Time[0] != nil || !*rows[1].Bool[2] {
				t.Fatalf("second link = %+v", rows[1])
			}
		}},
		{name: "states keep a missing transition", kind: "states", input: states, check: func(t *testing.T, rows []viewstore.BoardViewRow) {
			if len(rows) != 2 || rows[0].Kind != "state" || *rows[0].ID != "Todo" || *rows[0].Text[0] != "Done" || rows[1].Text[0] != nil || !*rows[1].Bool[0] || rows[1].Bool[2] == nil {
				t.Fatalf("states = %+v", rows)
			}
		}},
		{name: "arrays of different length", kind: "links", input: short, err: viewstore.ErrInvalidView},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			rows, err := expansions[test.kind].rows(&test.input)
			if !errors.Is(err, test.err) {
				t.Fatalf("err = %v, want %v", err, test.err)
			}
			if test.check != nil {
				test.check(t, rows)
			}
		})
	}
}
