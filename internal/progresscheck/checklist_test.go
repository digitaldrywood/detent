package progresscheck

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestFreezeChecklist(t *testing.T) {
	cfg := config.Default()
	cfg.Policy.ID = "test-policy"
	cfg.Gate.Run = "make check"
	cfg.Gate.SecurityAudit.Enabled = false
	for _, test := range []struct {
		name, verification string
		kinds              []tracker.ExecutionCheckKind
	}{
		{name: "deduplicate policy reference", verification: "Run `make check`.", kinds: []tracker.ExecutionCheckKind{tracker.CheckAcceptance, tracker.CheckAcceptance, tracker.CheckProjectGate}},
		{name: "unmapped command is review only", verification: "Run `make other`.", kinds: []tracker.ExecutionCheckKind{tracker.CheckAcceptance, tracker.CheckAcceptance, tracker.CheckVerification, tracker.CheckProjectGate}},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := "## Acceptance criteria\n\n- Same text\n- Same text\n\n## How we know it worked\n\n" + test.verification
			list, err := FreezeChecklist(body, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(list.Checks) != len(test.kinds) {
				t.Fatalf("checks=%+v, want kinds %v", list.Checks, test.kinds)
			}
			for i, check := range list.Checks {
				if check.Ordinal != i+1 || check.Kind != test.kinds[i] {
					t.Fatalf("check[%d]=%+v; want kind %s", i, check, test.kinds[i])
				}
				if check.Kind == tracker.CheckProjectGate {
					if check.CheckDigest == "" || check.Status != tracker.CheckUnverified {
						t.Fatalf("project gate lacks its trusted binding: %+v", check)
					}
				} else if check.CheckDigest != "" || check.Status != tracker.CheckNeedsReview {
					t.Fatalf("unmapped criterion acquired execution authority: %+v", check)
				}
			}
			changed, err := FreezeChecklist(body+" changed", cfg)
			if err != nil || changed.Revision == list.Revision {
				t.Fatalf("issue edit preserved its checklist revision: %+v, %v", changed, err)
			}
		})
	}
}
