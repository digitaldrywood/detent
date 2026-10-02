package workspace

import (
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"strconv"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/sys/unix"
)

func TestDarwinScratchEnvironmentProcessIDsScanBudget(t *testing.T) {
	t.Parallel()
	for _, inspectionErr := range []error{unix.EINVAL, unix.EIO, unix.EPERM, unix.EACCES, unix.ESRCH} {
		for _, cwdOwned := range []bool{false, true} {
			for _, survives := range []bool{false, true} {
				t.Run(inspectionErr.Error()+"/cwd="+strconv.FormatBool(cwdOwned)+"/survives="+strconv.FormatBool(survives), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						const ownedPID, unreadablePID = 2081001, 2081002
						const root = "/worker-scratch"
						processes := make([]unix.KinfoProc, 2)
						processes[0].Proc.P_pid = unreadablePID
						processes[1].Proc.P_pid = ownedPID
						environment := binary.LittleEndian.AppendUint32(nil, 0)
						environment = append(environment, "executable\x00DETENT_WORKER_SCRATCH="+root+"\x00"...)
						reads, scans := 0, 0
						start := time.Now()
						scan := func(ctx context.Context, root string) ([]int, error) {
							scans++
							if scans > 2 || scans > 1 && !survives {
								return nil, nil
							}
							return workspaceProcessIDsWithScanners(ctx, root,
								func(ctx context.Context, root string) ([]int, error) {
									return darwinScratchEnvironmentProcessIDs(ctx, root, processes, func(pid int) ([]byte, error) {
										if pid == unreadablePID {
											reads++
											return nil, inspectionErr
										}
										return environment, nil
									})
								}, func(context.Context, string) ([]int, error) {
									if cwdOwned {
										return []int{unreadablePID}, nil
									}
									return nil, nil
								})
						}
						var signaled []int
						var signals []syscall.Signal
						reaped, err := reapProcessesWithWait(t.Context(), root, time.Second, scan, func(pid int, sig syscall.Signal) error {
							signaled = append(signaled, pid)
							signals = append(signals, sig)
							return nil
						}, func(ctx context.Context, root string, _ time.Duration, scan workspaceProcessScanner) ([]int, error) {
							return scanOwnedWorkspaceProcessIDs(ctx, root, scan)
						})
						want := []int{ownedPID}
						if cwdOwned {
							want = append(want, unreadablePID)
						}
						wantReaped, wantReads := len(want), 1
						if survives {
							want = append(want, want...)
							wantReads = 2
							if signals[len(signals)-1] != syscall.SIGKILL {
								t.Fatalf("surviving owned processes did not receive SIGKILL: %v", signals)
							}
						}
						if err != nil || reaped != wantReaped || !reflect.DeepEqual(signaled, want) || reads != wantReads || time.Since(start) != 0 {
							t.Fatalf("reaped=%d error=%v signals=%v reads=%d elapsed=%v; want signals=%v, one immediate read per scan", reaped, err, signaled, reads, time.Since(start), want)
						}
					})
				})
			}
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
				return nil, unix.ENOMEM
			}
			return environment, nil
		})
	if !reflect.DeepEqual(pids, []int{ownedPID}) {
		t.Fatalf("process IDs = %v, want [%d]", pids, ownedPID)
	}
	if !errors.Is(err, unix.ENOMEM) {
		t.Fatalf("error = %v, want ENOMEM", err)
	}
}
