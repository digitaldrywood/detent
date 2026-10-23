//go:build unix

package procgroup

import "testing"

func TestParseProcessGroupRSS(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		output  string
		groupID int
		want    uint64
	}{
		{name: "sums group members", output: " 10 512\n 20 1024\n 10 256\n", groupID: 10, want: 768 * 1024},
		{name: "missing group", output: " 20 1024\n", groupID: 10, want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseProcessGroupRSS(test.output, test.groupID)
			if err != nil {
				t.Fatalf("parseProcessGroupRSS() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("parseProcessGroupRSS() = %d, want %d", got, test.want)
			}
		})
	}
}
