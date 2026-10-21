package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// The `usage:` section of the hosted configuration is the hub's price table
// (decisions section 17.5).

func TestReadHostedUsageConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		file      string
		wantErr   bool
		wantUnset bool
		wantPrice float64
		wantUnit  string
	}{
		{name: "absent section", file: "organization_id: org_1\n", wantUnset: true},
		{
			name: "prices",
			file: `usage:
  currency: USD
  prices:
    gpt-6-astra:
      input: 1.25
      cached_input: 0.125
      output: 10
    claude-opus-5:
      input: 5
      cached_input: 0.5
      output: 25
      currency: usd
`,
			wantPrice: 1.25, wantUnit: "USD",
		},
		{name: "empty price table", file: "usage:\n  prices: {}\n", wantUnset: true},
		{name: "negative price", file: "usage:\n  prices:\n    gpt-6-astra:\n      input: -1\n", wantErr: true},
		{name: "unnamed model", file: "usage:\n  prices:\n    \"\":\n      input: 1\n", wantErr: true},
		{name: "unknown field", file: "usage:\n  prices:\n    gpt-6-astra:\n      inputs: 1\n", wantErr: true},
		{name: "models differing by case", file: "usage:\n  prices:\n    gpt-6-astra:\n      input: 1\n    GPT-6-Astra:\n      input: 2\n", wantErr: true},
		{name: "models differing by whitespace", file: "usage:\n  prices:\n    gpt-6-astra:\n      input: 1\n    \" gpt-6-astra \":\n      input: 2\n", wantErr: true},
		{name: "mixed currencies", file: "usage:\n  currency: USD\n  prices:\n    gpt-6-astra:\n      input: 1\n    claude-opus-5:\n      input: 1\n      currency: EUR\n", wantErr: true},
		{name: "bad currency", file: "usage:\n  currency: dollars\n  prices:\n    gpt-6-astra:\n      input: 1\n", wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "hosted.yaml")
			if err := os.WriteFile(path, []byte(test.file), 0o600); err != nil {
				t.Fatal(err)
			}
			config, priced, err := readHostedUsageConfig(path)
			if test.wantErr {
				if err == nil {
					t.Fatalf("readHostedUsageConfig() = %+v, want an error", config)
				}
				return
			}
			if err != nil {
				t.Fatalf("readHostedUsageConfig() = %v, want nil", err)
			}
			if priced == test.wantUnset {
				t.Fatalf("readHostedUsageConfig() priced = %t, want %t", priced, !test.wantUnset)
			}
			if test.wantUnset {
				if len(config.Prices) != 0 {
					t.Fatalf("readHostedUsageConfig() = %+v, want no price table", config)
				}
				return
			}
			price, found := config.Prices["gpt-6-astra"]
			if !found || price.Input != test.wantPrice || price.Currency != test.wantUnit {
				t.Fatalf("gpt-6-astra = %+v (found %t), want %v %s", price, found, test.wantPrice, test.wantUnit)
			}
			if other := config.Prices["claude-opus-5"]; other.Currency != "USD" {
				t.Fatalf("claude-opus-5 currency = %q, want USD", other.Currency)
			}
		})
	}
}

func TestReadHostedUsageConfigWithoutAPath(t *testing.T) {
	t.Parallel()
	config, priced, err := readHostedUsageConfig("")
	if err != nil || priced || len(config.Prices) != 0 {
		t.Fatalf("readHostedUsageConfig(\"\") = %+v, %t, %v, want an empty table", config, priced, err)
	}
}
