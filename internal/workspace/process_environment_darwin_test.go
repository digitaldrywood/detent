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
		always     bool
		firstDelay time.Duration
		retryFor   time.Duration
		want       error
		wantReads  int
	}{
		{name: "exec transition", first: unix.EIO, alive: true, wantReads: 2},
		{name: "stack transition", first: unix.EINVAL, alive: true, wantReads: 2},
		{name: "loaded stack transition", first: unix.EINVAL, alive: true, firstDelay: 150 * time.Millisecond, wantReads: 2},
		{name: "stalled transition", first: unix.EINVAL, alive: true, always: true, retryFor: 10 * time.Millisecond, want: context.DeadlineExceeded},
		{name: "exited", first: unix.EINVAL, wantReads: 1},
		{name: "missing", first: unix.ESRCH, wantReads: 1},
		{name: "permission denied", first: unix.EPERM, alive: true, want: unix.EPERM, wantReads: 1},
		{name: "inspection failed", first: unix.EIO, inspectErr: unix.EPERM, want: unix.EPERM, wantReads: 1},
		{name: "bounded cancellation", first: unix.EIO, alive: true, cancel: true, want: context.Canceled, wantReads: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			retryFor := tt.retryFor
			if retryFor == 0 {
				retryFor = darwinScratchEnvironmentRetryTimeout
			}
			reads := 0
			data, err := readDarwinScratchEnvironment(ctx, retryFor, func() ([]byte, error) {
				reads++
				if reads == 1 || tt.always {
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
			if !errors.Is(err, tt.want) || tt.wantReads > 0 && reads != tt.wantReads {
				t.Fatalf("error = %v, reads = %d; want error %v, reads %d", err, reads, tt.want, tt.wantReads)
			}
			if tt.wantReads == 2 && string(data) != "environment" {
				t.Fatalf("environment = %q", data)
			}
		})
	}
}

func TestDarwinScratchEnvironmentProcessIDsContinuesAfterInspectionFailure(t *testing.T) {
	const (
		unreadablePID = 2081001
		ownedPID      = 2081002
	)
	root := t.TempDir()
	processes := make([]unix.KinfoProc, 2)
	processes[0].Proc.P_pid = unreadablePID
	processes[1].Proc.P_pid = ownedPID

	environment := binary.LittleEndian.AppendUint32(nil, 0)
	environment = append(environment, "executable\x00DETENT_WORKER_SCRATCH="+root+"\x00"...)
	pids, err := darwinScratchEnvironmentProcessIDs(t.Context(), root, processes, 10*time.Millisecond,
		func(pid int) ([]byte, error) {
			if pid == unreadablePID {
				return nil, unix.EINVAL
			}
			return environment, nil
		}, func(int) (bool, error) {
			return true, nil
		})
	if !reflect.DeepEqual(pids, []int{ownedPID}) {
		t.Fatalf("process IDs = %v, want [%d]", pids, ownedPID)
	}
	if !errors.Is(err, unix.EINVAL) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want EINVAL and deadline exceeded", err)
	}
}
