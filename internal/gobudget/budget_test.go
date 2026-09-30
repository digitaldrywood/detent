package gobudget

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewDefaultsSlotsToCPUCount(t *testing.T) {
	tests := []struct {
		name  string
		slots int
		want  int
	}{
		{name: "unset", slots: 0, want: runtime.NumCPU()},
		{name: "negative", slots: -3, want: runtime.NumCPU()},
		{name: "configured", slots: 6, want: 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := New(tt.slots, "dir", "exe").Slots; got != tt.want {
				t.Fatalf("New(%d).Slots = %d, want %d", tt.slots, got, tt.want)
			}
		})
	}
}

func TestWidthDividesBudgetAcrossInvocations(t *testing.T) {
	tests := []struct {
		slots int
		want  int
	}{
		{slots: 1, want: 1},
		{slots: 4, want: 1},
		{slots: 8, want: 2},
		{slots: 16, want: 4},
		{slots: 18, want: 4},
	}
	for _, tt := range tests {
		if got := (Budget{Slots: tt.slots}).Width(); got != tt.want {
			t.Fatalf("Width() with %d slots = %d, want %d", tt.slots, got, tt.want)
		}
	}
}

func TestGOFLAGSPreservesInheritedFlags(t *testing.T) {
	tests := []struct {
		name      string
		inherited string
		wrapper   string
		want      string
	}{
		{name: "empty", wrapper: "/b/w", want: "-p=4 -toolexec=/b/w"},
		{name: "inherited", inherited: "-mod=mod  -trimpath", wrapper: "/b/w", want: "-mod=mod -trimpath -p=4 -toolexec=/b/w"},
		{name: "explicit p", inherited: "-p=2", wrapper: "/b/w", want: "-p=2 -toolexec=/b/w"},
		{name: "explicit double dash toolexec", inherited: "--toolexec=/x", wrapper: "/b/w", want: "--toolexec=/x -p=4"},
		{name: "wrapper with space", wrapper: "/b c/w", want: "-p=4"},
		{name: "similar prefix is not p", inherited: "-pgo=off", wrapper: "/b/w", want: "-pgo=off -p=4 -toolexec=/b/w"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := goflags(tt.inherited, "4", tt.wrapper); got != tt.want {
				t.Fatalf("goflags() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnvironmentPreparesSlotsAndWrapper(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "budget")
	executable := filepath.Join(t.TempDir(), "detent")
	budget := New(8, dir, executable)

	for range 2 {
		environment, err := budget.Environment("-mod=mod")
		if err != nil {
			t.Fatalf("Environment() error = %v", err)
		}
		want := map[string]string{
			"GOFLAGS":        "-mod=mod -p=2 -toolexec=" + budget.WrapperPath(),
			"GOMAXPROCS":     "2",
			DirEnvironment:   dir,
			SlotsEnvironment: "8",
		}
		for key, value := range want {
			if environment[key] != value {
				t.Fatalf("Environment()[%s] = %q, want %q", key, environment[key], value)
			}
		}
	}
	for slot := range 8 {
		if _, err := os.Stat(slotPath(dir, slot)); err != nil {
			t.Fatalf("slot %d missing: %v", slot, err)
		}
	}
	target, err := os.Readlink(budget.WrapperPath())
	if err != nil || target != executable {
		t.Fatalf("wrapper link = %q, %v; want %q", target, err, executable)
	}

	relinked := filepath.Join(t.TempDir(), "detent-updated")
	if _, err := New(8, dir, relinked).Environment(""); err != nil {
		t.Fatalf("Environment() relink error = %v", err)
	}
	if target, _ := os.Readlink(budget.WrapperPath()); target != relinked {
		t.Fatalf("wrapper link after relink = %q, want %q", target, relinked)
	}
}

func TestEnvironmentDisabledWithoutLocation(t *testing.T) {
	tests := []Budget{
		{Slots: 4, Executable: "exe"},
		{Slots: 4, Dir: "dir"},
		{Dir: "dir", Executable: "exe"},
	}
	for _, budget := range tests {
		environment, err := budget.Environment("")
		if err != nil || len(environment) != 0 {
			t.Fatalf("Environment(%+v) = %v, %v; want empty", budget, environment, err)
		}
	}
}

func TestEnvironmentReportsUnwritableDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := New(2, filepath.Join(file, "budget"), "exe").Environment("")
	if err == nil || !strings.Contains(err.Error(), "go build budget") {
		t.Fatalf("Environment() error = %v, want go build budget error", err)
	}
}
