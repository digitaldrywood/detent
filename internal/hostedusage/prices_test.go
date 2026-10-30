package hostedusage

import "testing"

func TestUsageConfigEstimate(t *testing.T) {
	t.Parallel()
	prices := Config{Currency: "USD", Prices: map[string]Price{
		"gpt-6-astra":   {Input: 10, CachedInput: 1, Output: 40},
		"claude-opus-5": {Input: 20, CachedInput: 2, Output: 100},
	}}.Normalized()
	cases := []struct {
		name                  string
		model                 string
		input, cached, output int64
		wantCost              float64
		wantFound             bool
		wantSaving            float64
	}{
		{
			name: "uncached input, cached input and output", model: "gpt-6-astra",
			input: 1_000_000, cached: 400_000, output: 100_000,
			// 600k uncached at 10, 400k cached at 1, 100k output at 40.
			wantCost: 6 + 0.4 + 4, wantFound: true, wantSaving: 0.4 * 9,
		},
		{name: "case insensitive", model: "GPT-6-Astra", input: 1_000_000, wantCost: 10, wantFound: true},
		{name: "unknown model", model: "gpt-7", input: 1_000_000},
		{
			name: "cached exceeds input", model: "gpt-6-astra", input: 100, cached: 400,
			// Savings cap cached input at the input total, as pricing does.
			wantCost: 100.0 / 1_000_000 * 1, wantFound: true, wantSaving: 100.0 / 1_000_000 * 9,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cost, currency, found := prices.Estimate(test.model, test.input, test.cached, test.output)
			if found != test.wantFound {
				t.Fatalf("estimate(%q) found = %t, want %t", test.model, found, test.wantFound)
			}
			if !found {
				return
			}
			if currency != "USD" {
				t.Fatalf("estimate(%q) currency = %q, want USD", test.model, currency)
			}
			if diff := cost - test.wantCost; diff > 1e-9 || diff < -1e-9 {
				t.Fatalf("estimate(%q) = %v, want %v", test.model, cost, test.wantCost)
			}
			if saving := prices.CacheSaving(test.model, test.input, test.cached); saving-test.wantSaving > 1e-9 || saving-test.wantSaving < -1e-9 {
				t.Fatalf("cacheSaving(%q, %d) = %v, want %v", test.model, test.cached, saving, test.wantSaving)
			}
		})
	}
	if saving := prices.CacheSaving("gpt-7", 1_000_000, 1_000_000); saving != 0 {
		t.Fatalf("cacheSaving for an unpriced model = %v, want 0", saving)
	}
}
