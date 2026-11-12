package workspacerelay

import "testing"

func TestRelayActorName(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		email string
		want  string
	}{
		{name: "dotted local part", email: "ada.lovelace@example.test", want: "Ada Lovelace"},
		{name: "underscores and dashes", email: "grace_brewster-murray@example.test", want: "Grace Brewster Murray"},
		{name: "single word", email: "ada@example.test", want: "Ada"},
		{name: "already capitalised", email: "Ada.Lovelace@example.test", want: "Ada Lovelace"},
		{name: "routing tag", email: "ada.lovelace+detent@example.test", want: "Ada Lovelace"},
		{name: "surrounding space", email: "  ada@example.test  ", want: "Ada"},
		{name: "no domain", email: "ada.lovelace", want: "Ada Lovelace"},
		{name: "separators only", email: "...@example.test", want: "...@example.test"},
		{name: "non-ascii", email: "ada.ñ@example.test", want: "Ada Ñ"},
		{name: "empty", email: "", want: ""},
		{name: "blank", email: "   ", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := ActorName(test.email); got != test.want {
				t.Fatalf("relayActorName(%q) = %q, want %q", test.email, got, test.want)
			}
		})
	}
}
