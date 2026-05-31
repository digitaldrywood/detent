package budget

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultPricingTable(t *testing.T) {
	t.Parallel()

	pricing := DefaultPricingTable()
	row, ok := pricing.Lookup(" GPT-5.5 ")
	if !ok {
		t.Fatal("DefaultPricingTable().Lookup() ok = false, want true")
	}
	assertInDelta(t, row.USDPerInputToken, 0.000005)
	assertInDelta(t, row.USDPerOutputToken, 0.000030)
}

func TestDecodePricing(t *testing.T) {
	t.Parallel()

	raw := []byte(`
models:
  gpt-test:
    input_usd_per_1m_tokens: "10000"
    output_usd_per_1m_tokens: 20000
  gpt-per-token:
    usd_per_input_token: "0.01"
    usd_per_output_token: 0.02
  invalid-row: bad
  missing-output:
    usd_per_input_token: 0.01
  negative:
    usd_per_input_token: -0.01
    usd_per_output_token: 0.02
`)

	pricing, err := DecodePricing(raw)
	if err != nil {
		t.Fatalf("DecodePricing() error = %v", err)
	}

	million, ok := pricing.Lookup("gpt-test")
	if !ok {
		t.Fatal("pricing.Lookup(gpt-test) ok = false, want true")
	}
	assertInDelta(t, million.USDPerInputToken, 0.01)
	assertInDelta(t, million.USDPerOutputToken, 0.02)

	perToken, ok := pricing.Lookup("GPT-PER-TOKEN")
	if !ok {
		t.Fatal("pricing.Lookup(GPT-PER-TOKEN) ok = false, want true")
	}
	assertInDelta(t, perToken.USDPerInputToken, 0.01)
	assertInDelta(t, perToken.USDPerOutputToken, 0.02)

	for _, model := range []string{"invalid-row", "missing-output", "negative"} {
		if _, ok := pricing.Lookup(model); ok {
			t.Fatalf("pricing.Lookup(%q) ok = true, want false", model)
		}
	}
}

func TestLoadPricing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "models.yaml")
	if err := os.WriteFile(path, []byte(`
models:
  gpt-file:
    usd_per_input_token: 0.03
    usd_per_output_token: 0.04
`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	pricing, err := LoadPricing(path)
	if err != nil {
		t.Fatalf("LoadPricing() error = %v", err)
	}
	row, ok := pricing.Lookup("gpt-file")
	if !ok {
		t.Fatal("pricing.Lookup(gpt-file) ok = false, want true")
	}
	assertInDelta(t, row.USDPerInputToken, 0.03)
	assertInDelta(t, row.USDPerOutputToken, 0.04)

	if _, err := LoadPricing(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("LoadPricing(missing) error = nil, want error")
	}
}

func TestPricingForConfigUsesEmbeddedDefaultPath(t *testing.T) {
	t.Parallel()

	pricing, err := PricingForConfig(Config{PricingPath: DefaultPricingPath})
	if err != nil {
		t.Fatalf("PricingForConfig() error = %v", err)
	}
	if _, ok := pricing.Lookup("gpt-5.5"); !ok {
		t.Fatal("PricingForConfig(default).Lookup(gpt-5.5) ok = false, want true")
	}
}
