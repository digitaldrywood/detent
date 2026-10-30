package operatorchanges

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type cursorSource struct{ Source }

func (s cursorSource) Change(context.Context, string, string) (tracker.ChangeRequest, error) {
	return tracker.ChangeRequest{ID: "change_example", CurrentVersion: "version_example"}, nil
}

func (s cursorSource) Archive(context.Context, string, string) (Source, error) {
	return s, nil
}

func (s cursorSource) ArchiveDescriptor(context.Context) *tracker.ArchiveDescriptor { return nil }

func TestFragmentCursorRejectsChangedSelection(t *testing.T) {
	for _, test := range []struct {
		name  string
		alter func(*readCursor)
	}{
		{"offset", func(c *readCursor) { c.Offset = -1 }},
		{"version", func(c *readCursor) { c.Version = "version_other" }},
		{"change", func(c *readCursor) { c.Change = "change_other" }},
		{"section", func(c *readCursor) { c.Section = "checks" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cursor := readCursor{Change: "change_example", Head: "version_example", Version: "version_example", Section: "reviews", Offset: 1, Digest: strings.Repeat("a", 64), Through: 1}
			test.alter(&cursor)
			raw, err := encodeCursor(cursor)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Read(t.Context(), cursorSource{}, operatortool.ChangeArguments{Section: "reviews", VersionID: "version_example", Cursor: raw}, operatortool.ChangeResult{})
			if !errors.Is(err, operatortool.ErrInvalidArguments) {
				t.Fatalf("altered fragment cursor error = %v, want invalid arguments", err)
			}
		})
	}
}
