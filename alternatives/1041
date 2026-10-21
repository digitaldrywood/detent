package config

import "testing"

func TestReleaseDeliveryContract(t *testing.T) {
	const delegation = "## Release delivery contract\nVerified release picks of reviewed develop fixes may receive the approved delivery contract.\n"
	const sections = "### Acceptance criteria\nDeliver only the verified source.\n### Must not break\nPreserve gates.\n### How we know it worked\nThe associated pick passes review."
	for _, test := range []struct {
		name, prompt string
		want         bool
	}{
		{"approved", delegation + sections, true},
		{"missing authorization", "## Release delivery contract\n" + sections, false},
		{"duplicate section", delegation + sections + "\n### Must not break\nOther criteria.", false},
		{"extra section", delegation + sections + "\n### Other\nExtra scope.", false},
		{"missing required section", delegation + "### Acceptance criteria\nDeliver.", false},
		{"configured sections", "## Issue Contract\n- Result\n- Evidence\n" + delegation + "### Result\nDeliver the associated pick.\n### Evidence\nReview it.", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			contract, err := ResolveIssueContract(test.prompt)
			if err != nil {
				t.Fatal(err)
			}
			body := contract.ReleaseDeliveryBody()
			if (body != "") != test.want {
				t.Fatalf("contract=%q want present=%t", body, test.want)
			}
		})
	}
}
