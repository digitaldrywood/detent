package cloudentry

import (
	"net/url"
	"testing"
)

func TestParseAuditFilter(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		query string
		limit int
		from  string
		to    string
	}{
		{"", 50, "", ""},
		{"limit=200", 200, "", ""},
		{"from=2026-10-01&to=2026-10-01", 50, "2026-10-01T00:00:00.000000000", "2026-10-01T23:59:59.999999999"},
		{"from=2026-10-01T08%3A00%3A00-04%3A00&to=2026-10-01T12%3A00%3A00.1Z", 50, "2026-10-01T12:00:00.000000000", "2026-10-01T12:00:00.100000000"},
	} {
		t.Run(test.query, func(t *testing.T) {
			query, err := url.ParseQuery(test.query)
			if err != nil {
				t.Fatal(err)
			}
			filter, err := parseAuditFilter(query)
			if err != nil || filter.Limit != test.limit || filter.From != test.from || filter.To != test.to {
				t.Fatalf("filter = %+v, %v", filter, err)
			}
		})
	}
}
