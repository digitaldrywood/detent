package hubserver

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestIsLocalFilesystem(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		flags uint32
		want  bool
	}{
		{name: "local", flags: unix.MNT_LOCAL, want: true},
		{name: "local with other flags", flags: unix.MNT_LOCAL | unix.MNT_RDONLY, want: true},
		{name: "network", flags: 0, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := isLocalFilesystem(test.flags); got != test.want {
				t.Fatalf("isLocalFilesystem(%d) = %t, want %t", test.flags, got, test.want)
			}
		})
	}
}
