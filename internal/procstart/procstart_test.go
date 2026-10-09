package procstart

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

func TestMatches(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process start identity is exact only on darwin and linux")
	}
	self, err := Identity(os.Getpid())
	if err != nil {
		t.Fatalf("Identity(self) error = %v", err)
	}
	exited := exec.Command("sleep", "30")
	if err := exited.Start(); err != nil {
		t.Fatal(err)
	}
	exitedIdentity, err := Identity(exited.Process.Pid)
	if err != nil {
		t.Fatalf("Identity(child) error = %v", err)
	}
	if exitedIdentity == self {
		t.Fatalf("child identity %q equals parent identity", exitedIdentity)
	}
	if err := exited.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = exited.Wait()

	tests := []struct {
		name     string
		pid      int
		recorded string
		want     bool
	}{
		{name: "live process", pid: os.Getpid(), recorded: self, want: true},
		{name: "reused pid", pid: os.Getpid(), recorded: exitedIdentity, want: false},
		{name: "exited process", pid: exited.Process.Pid, recorded: exitedIdentity, want: false},
		{name: "missing identity", pid: os.Getpid(), recorded: "", want: false},
		{name: "invalid pid", pid: 0, recorded: self, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Matches(test.pid, test.recorded)
			if err != nil {
				t.Fatalf("Matches() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("Matches(%d, %q) = %t, want %t", test.pid, test.recorded, got, test.want)
			}
		})
	}
}
