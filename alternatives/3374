package web

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

func TestFleetActionProposalRejectsNonObjectArguments(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "null", raw: `null`},
		{name: "null with whitespace", raw: " \nnull\t "},
		{name: "array", raw: `[]`},
		{name: "string", raw: `"refresh"`},
		{name: "malformed", raw: `{`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server := &Server{}
			_, err := server.fleetActionProposal(t.Context(), operatortool.Refresh, json.RawMessage(test.raw))
			if !errors.Is(err, operatortool.ErrInvalidArguments) {
				t.Fatalf("fleetActionProposal() error = %v, want %v", err, operatortool.ErrInvalidArguments)
			}
		})
	}
}
