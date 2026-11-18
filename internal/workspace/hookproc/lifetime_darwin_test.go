package hookproc

import (
	"os"
	"os/exec"
	"testing"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

func TestLifetimeIdentifiesOnlyItsWriter(t *testing.T) {
	newLifetime := func() *Lifetime {
		t.Helper()
		l, err := NewLifetime()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := l.Close(); err != nil {
				t.Error(err)
			}
		})
		return l
	}
	l, other := newLifetime(), newLifetime()
	for _, tc := range []struct {
		name string
		file *os.File
		want bool
	}{
		{"writer", l.writer, true},
		{"reader", l.reader, false},
		{"other writer", other.writer, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "/bin/cat")
			cmd.ExtraFiles = []*os.File{nil, tc.file}
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := procgroup.Start(cmd); err != nil {
				t.Fatal(err)
			}
			defer func() {
				input.Close()
				if err := cmd.Wait(); err != nil {
					t.Error(err)
				}
			}()
			got, err := l.holdsWriter(t.Context(), cmd.Process.Pid)
			if err != nil || got != tc.want {
				t.Fatalf("holdsWriter = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}
