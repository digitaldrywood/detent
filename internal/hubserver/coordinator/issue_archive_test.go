package coordinator

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidateCoordinatorArchiveIDs(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		ids   []string
		valid bool
	}{
		{"one issue", []string{"wi_one"}, true},
		{"empty", nil, false},
		{"duplicate", []string{"wi_one", "wi_one"}, false},
		{"number", []string{"#1"}, false},
		{"unbounded ID", []string{"wi_" + strings.Repeat("x", 256)}, false},
		{"maximum", archiveTestIDs(ArchiveMaxItems), true},
		{"too many", archiveTestIDs(ArchiveMaxItems + 1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateArchiveIDs(test.ids); (err == nil) != test.valid {
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
