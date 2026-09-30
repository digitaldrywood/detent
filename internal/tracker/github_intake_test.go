package tracker

import "testing"

func TestParseGitHubIssueURL(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		raw   string
		valid bool
	}{
		{"https://github.com/Acme/Orders/issues/12", true},
		{"github.com/acme/orders/issues/12/", true},
		{"http://github.com/acme/orders/issues/12", false},
		{"https://github.com/acme/orders/pull/12", false},
		{"https://github.com/acme/orders/issues/0", false},
		{"https://github.com/acme/orders/issues/-1", false},
		{"https://github.com/acme/orders/issues/999999999999999999999999", false},
		{"https://github.com.evil/acme/orders/issues/12", false},
		{"https://user@github.com/acme/orders/issues/12", false},
		{"https://github.com:443/acme/orders/issues/12", false},
		{"https://github.com/acme/orders/issues/12?x=1", false},
		{"https://github.com/acme/orders/issues/12#comment", false},
		{"https://github.com/acme/orders/issues/%31%32", false},
		{"https://github.com/acme/../issues/12", false},
	} {
		t.Run(test.raw, func(t *testing.T) {
			canonical, repo, number, err := ParseGitHubIssueURL(test.raw)
			if (err == nil) != test.valid {
				t.Fatalf("valid = %v, error = %v", test.valid, err)
			}
			if test.valid && (canonical != "https://github.com/acme/orders/issues/12" || repo != "acme/orders" || number != 12) {
				t.Fatalf("identity = %s %s %d", canonical, repo, number)
			}
		})
	}
}
