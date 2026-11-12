package workspacehub

import (
	"slices"
	"testing"
)

func TestRelayedRequiresDropsOnlyUnservedSurfaces(t *testing.T) {
	t.Parallel()
	tests := []struct {
		requires []string
		want     []string
	}{
		{requires: []string{"files", "exec"}, want: []string{"files", "exec"}},
		{requires: []string{"files", "diff"}, want: []string{"files"}},
		{requires: []string{"terminal", "preview", "git"}, want: []string{"terminal", "git"}},
	}
	for _, test := range tests {
		if got := RelayedRequires(test.requires); !slices.Equal(got, test.want) {
			t.Errorf("relayedRequires(%v) = %v, want %v", test.requires, got, test.want)
		}
	}
}
