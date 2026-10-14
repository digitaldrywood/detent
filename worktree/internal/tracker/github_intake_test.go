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

func TestAppendGitHubIssueClosingReferences(t *testing.T) {
	t.Parallel()
	imported := GitHubIssueSourceReference("I_original", "https://github.com/digitaldrywood/detent/issues/3410")
	linked := GitHubIssueSourceReference("https://github.com/acme/orders/issues/12", "https://github.com/acme/orders/issues/12")
	mismatch := imported
	mismatch.Number = 29
	for _, test := range []struct {
		name    string
		body    string
		sources []ExternalReference
		want    string
	}{
		{name: "native only", body: "Native #29\n\nMention Closes acme/orders#99 in the source body", want: "Native #29\n\nMention Closes acme/orders#99 in the source body"},
		{name: "imported number", body: "Native #29", sources: []ExternalReference{imported}, want: "Native #29\n\nCloses digitaldrywood/detent#3410"},
		{name: "linked and imported duplicates", body: "Change", sources: []ExternalReference{imported, linked, imported}, want: "Change\n\nCloses acme/orders#12\nCloses digitaldrywood/detent#3410"},
		{name: "preserves existing attribution", body: "Human attribution\n\nCloses acme/orders#12", sources: []ExternalReference{linked, imported}, want: "Human attribution\n\nCloses acme/orders#12\n\nCloses digitaldrywood/detent#3410"},
		{name: "same reference is unchanged", body: "Human attribution\n\ncloses ACME/ORDERS#12", sources: []ExternalReference{linked}, want: "Human attribution\n\ncloses ACME/ORDERS#12"},
		{name: "opaque or contradictory metadata", body: "Change", sources: []ExternalReference{GitHubIssueSourceReference("I_only", ""), mismatch, {Provider: "github", Kind: "pull_request", URL: imported.URL, Repository: imported.Repository, Number: imported.Number}}, want: "Change"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := AppendGitHubIssueClosingReferences(test.body, test.sources); got != test.want {
				t.Fatalf("body = %q, want %q", got, test.want)
			}
		})
	}
}
