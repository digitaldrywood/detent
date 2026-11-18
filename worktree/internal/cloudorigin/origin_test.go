package cloudorigin

import "testing"

func TestAliasesRejectsUnrecognizedPairs(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, from, to string
	}{
		{"same production", Production, Production},
		{"same staging", Staging, Staging},
		{"same legacy", "https://hub.detent.build", "https://hub.detent.build"},
		{"canonical environments", Production, Staging},
		{"legacy environments", "https://hub.detent.build", "https://staging.hub.detent.build"},
		{"empty canonical", "", Production},
		{"empty unknown", "", "https://tenant.example.test"},
		{"customer pair", "https://cloud.example.test", "https://hub.example.test"},
		{"trailing slash", Production + "/", "https://hub.detent.build"},
		{"insecure legacy", Production, "http://hub.detent.build"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if Aliases(test.from, test.to) || Aliases(test.to, test.from) {
				t.Fatalf("unexpected alias between %q and %q", test.from, test.to)
			}
		})
	}
}
