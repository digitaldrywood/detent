package hookproc

import (
	"io"
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
	if active, err := l.Active(); err != nil || active {
		t.Fatalf("unstarted hook has a writer: active = %t, error = %v", active, err)
	}
	otherWriter, err := os.OpenFile(other.path(), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { otherWriter.Close() })
	readWrite, err := os.OpenFile(l.path(), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { readWrite.Close() })
	for _, tc := range []struct {
		name string
		file *os.File
		want bool
	}{
		{"writer", nil, true},
		{"reader", l.reader, false},
		{"read-write descriptor", readWrite, false},
		{"other writer", otherWriter, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "/bin/cat")
			if tc.want {
				(&Owner{descriptor: l}).ConfigureCommand(cmd)
			} else {
				cmd.ExtraFiles = []*os.File{nil, tc.file}
			}
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := cmd.StdoutPipe()
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
			if _, err := io.WriteString(input, "ready\n"); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(output, make([]byte, len("ready\n"))); err != nil {
				t.Fatal(err)
			}
			got, err := l.holdsWriter(t.Context(), cmd.Process.Pid)
			if err != nil || got != tc.want {
				t.Fatalf("holdsWriter = %v, %v; want %v", got, err, tc.want)
			}
			if tc.want {
				marker, err := l.reader.Stat()
				if err != nil {
					t.Fatal(err)
				}
				if marker.Mode().Perm() != 0o400 {
					t.Fatalf("launched hook FIFO permissions = %o", marker.Mode().Perm())
				}
			}
		})
	}
}
