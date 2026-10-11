//go:build unix

package gobudget

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIsWrapperInvocation(t *testing.T) {
	tests := []struct {
		arg0 string
		want bool
	}{
		{arg0: "/tmp/budget/bin/detent-go-budget", want: true},
		{arg0: "detent-go-budget.exe", want: true},
		{arg0: "/usr/local/bin/detent", want: false},
		{arg0: "", want: false},
	}
	for _, tt := range tests {
		if got := IsWrapperInvocation(tt.arg0); got != tt.want {
			t.Fatalf("IsWrapperInvocation(%q) = %v, want %v", tt.arg0, got, tt.want)
		}
	}
}

func TestGatedOnlyForBuildTools(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{args: []string{"/go/pkg/tool/compile", "-o", "x.a"}, want: true},
		{args: []string{"/go/pkg/tool/link.exe", "-o", "x"}, want: true},
		{args: []string{"/go/pkg/tool/asm"}, want: true},
		{args: []string{"/go/pkg/tool/cgo", "-objdir", "o"}, want: true},
		{args: []string{"/go/pkg/tool/vet", "vet.cfg"}, want: true},
		{args: []string{"/go/pkg/tool/compile", "-V=full"}, want: false},
		{args: []string{"/go/pkg/tool/vet", "-V"}, want: false},
		{args: []string{"clang", "-###", "-x", "c"}, want: false},
		{args: []string{"/go/pkg/tool/buildid", "x"}, want: false},
	}
	for _, tt := range tests {
		if got := gated(tt.args); got != tt.want {
			t.Fatalf("gated(%q) = %v, want %v", tt.args, got, tt.want)
		}
	}
}

func TestAcquireFailsOpenWithoutSlots(t *testing.T) {
	tests := []struct {
		name  string
		dir   string
		slots string
	}{
		{name: "no dir", slots: "2"},
		{name: "invalid slots", dir: t.TempDir(), slots: "many"},
		{name: "zero slots", dir: t.TempDir(), slots: "0"},
		{name: "missing slot files", dir: t.TempDir(), slots: "2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sleep := func(time.Duration) { t.Fatal("acquire waited instead of failing open") }
			if err := acquire(tt.dir, tt.slots, sleep)(); err != nil {
				t.Fatalf("release error = %v", err)
			}
		})
	}
}

func TestAcquireQueuesUntilSlotFrees(t *testing.T) {
	dir := t.TempDir()
	budget := New(1, dir, "exe")
	if err := budget.prepare(); err != nil {
		t.Fatal(err)
	}
	holder := acquire(dir, "1", func(time.Duration) { t.Fatal("first acquire waited") })

	var waits []time.Duration
	sleep := func(wait time.Duration) {
		waits = append(waits, wait)
		if len(waits) == 6 {
			if err := holder(); err != nil {
				t.Fatalf("release holder: %v", err)
			}
		}
	}
	if err := acquire(dir, "1", sleep)(); err != nil {
		t.Fatalf("release waiter: %v", err)
	}

	want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond, 160 * time.Millisecond, 250 * time.Millisecond}
	if len(waits) != len(want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("waits = %v, want %v", waits, want)
		}
	}
}

func TestAcquireUsesAnyFreeSlot(t *testing.T) {
	dir := t.TempDir()
	if err := New(3, dir, "exe").prepare(); err != nil {
		t.Fatal(err)
	}
	noWait := func(time.Duration) { t.Fatal("acquire waited with a free slot") }
	releases := []func() error{acquire(dir, "3", noWait), acquire(dir, "3", noWait), acquire(dir, "3", noWait)}
	for _, release := range releases {
		if err := release(); err != nil {
			t.Fatalf("release error = %v", err)
		}
	}
}

func TestRunWrapperPassesThroughToolResult(t *testing.T) {
	if testing.Short() {
		t.Skip("toolchain subprocess wrapper integration")
	}

	script := filepath.Join(t.TempDir(), "compile")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"out:$*\"\necho err >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	getenv := func(string) string { return "" }

	code := RunWrapper([]string{script, "-o", "x.a"}, getenv, strings.NewReader(""), &stdout, &stderr)

	if code != 3 || stdout.String() != "out:-o x.a\n" || stderr.String() != "err\n" {
		t.Fatalf("RunWrapper() = %d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunWrapperReportsStartFailures(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{name: "no tool", want: 2},
		{name: "missing tool", args: []string{filepath.Join(t.TempDir(), "compile")}, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := RunWrapper(tt.args, func(string) string { return "" }, nil, &bytes.Buffer{}, &stderr)
			if code != tt.want || !strings.Contains(stderr.String(), WrapperName) {
				t.Fatalf("RunWrapper() = %d stderr=%q, want %d", code, stderr.String(), tt.want)
			}
		})
	}
}
