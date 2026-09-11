package workspace

import (
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestDarwinProcessEnvironment(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		args uint32
		data string
		want []string
	}{
		{name: "truncated"},
		{name: "missing arguments", args: 2, data: "exe\x00\x00argv0\x00"},
		{name: "empty argument", args: 2, data: "exe\x00\x00argv0\x00\x00TMPDIR=/scratch\x00", want: []string{"TMPDIR=/scratch", ""}},
		{name: "argument is not environment", args: 2, data: "exe\x00argv0\x00TMPDIR=/argument\x00TMPDIR=/scratch\x00", want: []string{"TMPDIR=/scratch", ""}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := binary.LittleEndian.AppendUint32(nil, tt.args)
			data = append(data, tt.data...)
			if got := darwinProcessEnvironment(data); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("environment = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadDarwinScratchEnvironment(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		first      error
		alive      bool
		inspectErr error
		cancel     bool
		firstDelay time.Duration
		want       error
		wantReads  int
	}{
		{name: "exec transition", first: unix.EIO, alive: true, wantReads: 2},
		{name: "stack transition", first: unix.EINVAL, alive: true, wantReads: 2},
		{name: "loaded stack transition", first: unix.EINVAL, alive: true, firstDelay: 150 * time.Millisecond, wantReads: 2},
		{name: "exited", first: unix.EINVAL, wantReads: 1},
		{name: "missing", first: unix.ESRCH, wantReads: 1},
		{name: "permission denied", first: unix.EPERM, alive: true, want: unix.EPERM, wantReads: 1},
		{name: "inspection failed", first: unix.EIO, inspectErr: unix.EPERM, want: unix.EPERM, wantReads: 1},
		{name: "bounded cancellation", first: unix.EIO, alive: true, cancel: true, want: context.Canceled, wantReads: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reads := 0
			data, err := readDarwinScratchEnvironment(ctx, func() ([]byte, error) {
				reads++
				if reads == 1 {
					if tt.firstDelay > 0 {
						// Model a loaded process whose user stack remains unavailable
						// beyond the former private 100 ms inspection deadline.
						timer := time.NewTimer(tt.firstDelay)
						select {
						case <-ctx.Done():
							timer.Stop()
							return nil, ctx.Err()
						case <-timer.C:
						}
					}
					if tt.cancel {
						cancel()
					}
					return nil, tt.first
				}
				return []byte("environment"), nil
			}, func() (bool, error) { return tt.alive, tt.inspectErr })
			if !errors.Is(err, tt.want) || reads != tt.wantReads {
				t.Fatalf("error = %v, reads = %d; want %v, %d", err, reads, tt.want, tt.wantReads)
			}
			if tt.wantReads == 2 && string(data) != "environment" {
				t.Fatalf("environment = %q", data)
			}
		})
	}
}
