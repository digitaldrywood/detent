//go:build unix

package procgroup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/digitaldrywood/detent/internal/shell"
)

func TestRSSInspectionDoesNotRegisterCommand(t *testing.T) {
	t.Parallel()
	identity, err := inspectProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	ctx := shell.WithCommandStarter(t.Context(), func(context.Context, *exec.Cmd) (func() error, error) {
		t.Error("RSS inspection registered a host command")
		return nil, errors.New("unexpected host command")
	})
	if rss, err := RSS(ctx, identity); err != nil || rss == 0 {
		t.Fatalf("RSS = %d, %v", rss, err)
	}
}

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
