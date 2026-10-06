package policy

import "testing"

func TestPlacementValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		placement Placement
		valid     bool
	}{
		{"legacy default", Placement{}, true},
		{"blended", Placement{Mode: "blended"}, true},
		{"blended concurrency cap", Placement{Mode: "blended", MaxSpriteSlots: 2}, true},
		{"invalid concurrency cap", Placement{Mode: "blended", MaxSpriteSlots: -1}, false},
		{"Sprites-only", Placement{Mode: "sprites_only"}, true},
		{"local-first explicit bounds", Placement{Mode: "local_first", TodoThreshold: 10, OverflowSlots: 2}, true},
		{"no implicit threshold", Placement{Mode: "local_first", OverflowSlots: 2}, false},
		{"no implicit overflow", Placement{Mode: "local_first", TodoThreshold: 10}, false},
		{"overflow bounded", Placement{Mode: "local_first", TodoThreshold: 10, OverflowSlots: 101}, false},
		{"blended rejects overflow settings", Placement{Mode: "blended", OverflowSlots: 2}, false},
		{"unrequested mode rejected", Placement{Mode: "local_only"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.placement.Validate(); (err == nil) != test.valid {
				t.Fatalf("Validate()=%v, valid=%t", err, test.valid)
			}
			if test.placement.Mode == "" && test.placement.Resolved().Mode != "blended" {
				t.Fatal("legacy placement changed")
			}
		})
	}
}
