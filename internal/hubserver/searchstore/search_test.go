package searchstore

import "testing"

func TestNumberQualifierPostgresCannotStore(t *testing.T) {
	for _, test := range []struct {
		query   string
		number  int64
		project string
	}{
		{"42", 42, ""},
		{"example#42", 42, "example"},
		{"exa\x00mple#42", 0, ""},
		{"\x00#42", 0, ""},
		{"\x00", 0, ""},
	} {
		number, project := Number(test.query)
		if number != test.number || project != test.project {
			t.Errorf("Number(%q)=(%d,%q) want (%d,%q)", test.query, number, project, test.number, test.project)
		}
	}
}

func TestWindowQueryQuotesTerms(t *testing.T) {
	for _, test := range []struct{ text, want string }{
		{"magazine count reason code", "'magazine':* | 'count':* | 'reason':* | 'code':*"},
		{"Count COUNT count", "'count':*"},
		{`o'brien back\slash`, `'o':* | 'brien':* | 'back':* | 'slash':*`},
		{"Café ÉCOLE", "'café':* | 'école':*"},
		{"!!! :* & | ( )", ""},
		{"", ""},
	} {
		if got := WindowQuery(test.text); got != test.want {
			t.Errorf("WindowQuery(%q)=%q want %q", test.text, got, test.want)
		}
	}
}
