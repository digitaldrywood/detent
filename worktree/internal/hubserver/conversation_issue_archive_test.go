package hubserver

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidateCoordinatorArchiveIDs(t *testing.T) {
	for _, test := range []struct {
		name  string
		ids   []string
		valid bool
	}{
		{"one issue", []string{"wi_one"}, true},
		{"multiple issues", []string{"wi_one", "wi_two"}, true},
		{"empty", nil, false},
		{"duplicate", []string{"wi_one", "wi_one"}, false},
		{"number", []string{"#1"}, false},
		{"missing ID", []string{""}, false},
		{"unbounded ID", []string{"wi_" + strings.Repeat("x", 256)}, false},
		{"maximum", archiveTestIDs(coordinatorArchiveMaxItems), true},
		{"too many", archiveTestIDs(coordinatorArchiveMaxItems + 1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateCoordinatorArchiveIDs(test.ids); (err == nil) != test.valid {
				t.Fatalf("validation = %v, valid=%v", err, test.valid)
			}
		})
	}
}

func archiveTestIDs(count int) []string {
	ids := make([]string, count)
	for i := range ids {
		ids[i] = fmt.Sprintf("wi_%d", i)
	}
	return ids
}
