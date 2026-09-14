package workspace

import (
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/sys/unix"
)

func TestDarwinScratchEnvironmentProcessIDsScanBudget(t *testing.T) {
	t.Parallel()
	for _, transient := range []error{unix.EINVAL, unix.EIO} {
		for _, tt := range []struct {
			name         string
			configured   bool
			stalled      bool
			cancelParent bool
			wantErr      error
		}{
			{name: "reconciliation recovers"},
			{name: "reaping recovers", configured: true},
			{name: "reconciliation stalls", stalled: true, wantErr: context.DeadlineExceeded},
			{name: "reaping stalls", configured: true, stalled: true, wantErr: context.DeadlineExceeded},
			{name: "caller cancels", cancelParent: true, wantErr: context.Canceled},
		} {
			t.Run(transient.Error()+"/"+tt.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					const ownedPID, transientPID = 2081001, 2081002
					const root = "/worker-scratch"
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					timeout := minimumWorkspaceProcessScanTimeout
					if tt.configured {
						timeout = 5 * time.Second
						ctx = withWorkspaceProcessScanBudget(ctx, timeout, context.WithTimeout)
					}
					processes := make([]unix.KinfoProc, 2)
					processes[0].Proc.P_pid = ownedPID
					processes[1].Proc.P_pid = transientPID
					environment := binary.LittleEndian.AppendUint32(nil, 0)
					environment = append(environment, "executable\x00DETENT_WORKER_SCRATCH="+root+"\x00"...)
					reads := 0
					cwdScanned := false
					start := time.Now()
					pids, err := workspaceProcessIDsWithScanners(ctx, root,
						func(ctx context.Context, root string) ([]int, error) {
							return darwinScratchEnvironmentProcessIDs(ctx, root, processes,
								func(pid int) ([]byte, error) {
									if pid == ownedPID {
										return environment, nil
									}
									reads++
									if reads == 1 {
										// Virtual time models host delay past the former private
										// deadline without depending on scheduler margins.
										time.Sleep(2 * time.Second)
										if tt.cancelParent {
											cancel()
										}
										return nil, transient
									}
									if tt.stalled {
										return nil, transient
									}
									return environment, nil
								}, func(int) (bool, error) { return true, nil })
						}, func(ctx context.Context, _ string) ([]int, error) {
							cwdScanned = true
							return nil, ctx.Err()
						})
					wantPIDs := []int{ownedPID}
					if tt.wantErr == nil {
						wantPIDs = append(wantPIDs, transientPID)
					}
					if !errors.Is(err, tt.wantErr) || !reflect.DeepEqual(pids, wantPIDs) || cwdScanned != (tt.wantErr == nil) {
						t.Fatalf("process IDs = %v, error = %v, cwd scanned = %t; want %v, %v, %t", pids, err, cwdScanned, wantPIDs, tt.wantErr, tt.wantErr == nil)
					}
					if tt.wantErr != nil && !errors.Is(err, transient) {
						t.Fatalf("error = %v, want original transient error %v", err, transient)
					}
					if tt.stalled && time.Since(start) != timeout {
						t.Fatalf("scan stopped after %v, want stage budget %v", time.Since(start), timeout)
					}
				})
			})
		}
	}
}

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
		timeout    time.Duration
		want       error
		wantAlso   error
		wantReads  int
	}{
		{name: "exec transition", first: unix.EIO, alive: true, wantReads: 2},
		{name: "stack transition", first: unix.EINVAL, alive: true, wantReads: 2},
		{name: "loaded stack transition", first: unix.EINVAL, alive: true, firstDelay: 2 * time.Second, wantReads: 2},
		{name: "stalled transition", first: unix.EINVAL, alive: true, always: true, timeout: time.Second, want: context.DeadlineExceeded, wantAlso: unix.EINVAL},
		{name: "exited", first: unix.EINVAL, wantReads: 1},
		{name: "missing", first: unix.ESRCH, wantReads: 1},
		{name: "permission denied", first: unix.EPERM, alive: true, want: unix.EPERM, wantReads: 1},
		{name: "inspection failed", first: unix.EIO, inspectErr: unix.EPERM, want: unix.EPERM, wantReads: 1},
		{name: "bounded cancellation", first: unix.EIO, alive: true, cancel: true, want: context.Canceled, wantReads: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				timeout := tt.timeout
				if timeout == 0 {
					timeout = minimumWorkspaceProcessScanTimeout
				}
				ctx, cancel := context.WithTimeout(t.Context(), timeout)
				defer cancel()
				reads := 0
				data, err := readDarwinScratchEnvironment(ctx, func() ([]byte, error) {
					reads++
					if reads == 1 || tt.always {
						if tt.firstDelay > 0 {
							// This delay advances only the synctest virtual clock.
							time.Sleep(tt.firstDelay)
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
				if tt.wantAlso != nil && !errors.Is(err, tt.wantAlso) {
					t.Fatalf("error = %v, want additional error %v", err, tt.wantAlso)
				}
				if tt.wantReads == 2 && string(data) != "environment" {
					t.Fatalf("environment = %q", data)
				}
			})
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
	pids, err := darwinScratchEnvironmentProcessIDs(t.Context(), root, processes,
		func(pid int) ([]byte, error) {
			if pid == unreadablePID {
				return nil, unix.EPERM
			}
			return environment, nil
		}, func(int) (bool, error) {
			return true, nil
		})
	if !reflect.DeepEqual(pids, []int{ownedPID}) {
		t.Fatalf("process IDs = %v, want [%d]", pids, ownedPID)
	}
	if !errors.Is(err, unix.EPERM) {
		t.Fatalf("error = %v, want EPERM", err)
	}
}
