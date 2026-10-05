package cloudentry

import (
	"slices"
	"strings"
	"testing"
)

func TestContentSecurityPolicy(t *testing.T) {
	t.Parallel()
	directives := map[string][]string{}
	for directive := range strings.SplitSeq(contentSecurity, ";") {
		fields := strings.Fields(directive)
		if len(fields) == 0 {
			continue
		}
		if _, ok := directives[fields[0]]; ok {
			t.Fatalf("directive %s is declared twice", fields[0])
		}
		directives[fields[0]] = fields[1:]
	}
	for _, test := range []struct {
		name      string
		directive string
		source    string
		allowed   bool
	}{
		{name: "scripts never run inline", directive: "script-src", source: "'unsafe-inline'", allowed: false},
		{name: "style elements come only from the bundle", directive: "style-src", source: "'unsafe-inline'", allowed: false},
		{name: "the bundle's own stylesheets load", directive: "style-src", source: "'self'", allowed: true},
		{name: "the diff renderer's inline style attributes apply", directive: "style-src-attr", source: "'unsafe-inline'", allowed: true},
		{name: "objects never load", directive: "object-src", source: "'none'", allowed: true},
		{name: "pages cannot be framed", directive: "frame-ancestors", source: "'none'", allowed: true},
		{name: "same-origin framing stays forbidden", directive: "frame-ancestors", source: "'self'", allowed: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			sources, ok := directives[test.directive]
			if !ok {
				t.Fatalf("policy has no %s directive: %s", test.directive, contentSecurity)
			}
			if got := slices.Contains(sources, test.source); got != test.allowed {
				t.Fatalf("%s contains %s = %v, want %v", test.directive, test.source, got, test.allowed)
			}
		})
	}
}
