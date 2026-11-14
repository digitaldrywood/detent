package hubapi_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttemptDiffBodyAllowanceCoversEscaping(t *testing.T) {
	t.Parallel()
	const sample = 1 << 10
	for _, test := range []struct {
		name string
		char string
	}{
		{name: "less than", char: "<"},
		{name: "greater than", char: ">"},
		{name: "ampersand", char: "&"},
		{name: "escape", char: "\x1b"},
		{name: "quote", char: "\""},
		{name: "newline", char: "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := json.Marshal(strings.Repeat(test.char, sample))
			if err != nil {
				t.Fatal(err)
			}
			if perByte := (len(encoded) - 2) / sample; perByte > 6 {
				t.Fatalf("%q encodes to %d bytes per byte, above the allowance factor", test.char, perByte)
			}
		})
	}
	if want := int64(6*tracker.MaxDiffBytes + (1 << 20)); hubapi.MaxAttemptDiffRequestBytes != want {
		t.Fatalf("allowance = %d, want %d", hubapi.MaxAttemptDiffRequestBytes, want)
	}
}
