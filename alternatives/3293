package templates

import (
	"bytes"
	"testing"
)

func TestAttemptCosts(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, metrics, token, compute, detail string }{
		{"measured", `{"total_tokens":41741,"token_usd":0.2255,"compute_usd":0.00012,"cpu_seconds":3.4,"avg_memory_bytes":430000000,"wall_seconds":10.1}`, "$0.23", "$0.000120", "3.40 CPU s"},
		{"macOS", `{"total_tokens":100,"token_usd":0.25}`, "$0.25", "Unavailable", ""},
		{"historical", `{"total_tokens":100}`, "Unavailable", "Unavailable", ""},
		{"free", `{"token_usd":0,"compute_usd":0}`, "$0.00", "$0.00", "0.00 CPU s"},
		{"corrupt", `invalid`, "Unavailable", "Unavailable", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			row := AttemptCost(42, 2, "code", "sprite", tt.metrics)
			if row.TokenUSD != tt.token || row.ComputeUSD != tt.compute {
				t.Fatalf("row = %+v", row)
			}
			var output bytes.Buffer
			if err := BoardAttemptCosts([]AttemptCostData{row}, "").Render(t.Context(), &output); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{`data-attempt-cost="42"`, "Attempt 2", "Token USD", "Compute USD", tt.token, tt.compute, tt.detail} {
				if !bytes.Contains(output.Bytes(), []byte(want)) {
					t.Fatalf("missing %q: %s", want, output.String())
				}
			}
		})
	}
}
