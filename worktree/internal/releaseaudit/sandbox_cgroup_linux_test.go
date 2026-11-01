package releaseaudit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxCgroupControls(t *testing.T) {
	for _, missing := range []string{"", "memory.max", "memory.swap.max", "memory.oom.group", "cgroup.kill"} {
		name := missing
		if name == "" {
			name = "complete"
		}
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			for _, control := range []string{"memory.max", "memory.swap.max", "memory.oom.group", "cgroup.kill"} {
				if control != missing {
					if err := os.WriteFile(filepath.Join(directory, control), []byte("max\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			group, err := configureSandboxCgroup(directory)
			if missing != "" {
				if err == nil || group != nil {
					t.Fatal("sandbox accepted an unavailable memory control")
				}
				if _, err := os.Stat(filepath.Join(directory, missing)); !os.IsNotExist(err) {
					t.Fatalf("sandbox created a missing kernel control: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := group.file.Close(); err != nil {
					t.Fatal(err)
				}
			}()
			for control, want := range map[string]string{"memory.max": "1073741824", "memory.swap.max": "0", "memory.oom.group": "1"} {
				content, err := os.ReadFile(filepath.Join(directory, control))
				if err != nil || strings.TrimSpace(string(content)) != want {
					t.Fatalf("%s = %q, %v; want %s", control, content, err, want)
				}
			}
		})
	}
}

func TestSandboxCgroupAttachmentFailureNeverExecutes(t *testing.T) {
	directory := t.TempDir()
	for _, control := range []string{"memory.max", "memory.swap.max", "memory.oom.group", "cgroup.kill", "memory.events"} {
		if err := os.WriteFile(filepath.Join(directory, control), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	group, err := configureSandboxCgroup(directory)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "executed")
	command := exec.CommandContext(t.Context(), "/bin/sh", "-c", `printf executed > "$1"`, "fixture", marker)
	if err := runSandboxCgroupCommand(command, group); err == nil {
		t.Fatal("sandbox accepted a directory that is not a kernel cgroup")
	}
	if command.Process != nil {
		t.Fatal("fixture process started before successful cgroup attachment")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("fixture executed after cgroup attachment failed: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(directory, "cgroup.kill"))
	if err != nil || string(content) != "1" {
		t.Fatalf("cgroup cleanup = %q, %v", content, err)
	}
}

func TestSandboxCgroupMemoryObservation(t *testing.T) {
	for _, test := range []struct {
		name   string
		events string
		valid  bool
	}{
		{name: "within budget", events: "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\noom_group_kill 0\n", valid: true},
		{name: "allocation failure", events: "oom 1\noom_kill 0\n"},
		{name: "child killed", events: "oom 0\noom_kill 1\n"},
		{name: "invalid event", events: "oom invalid\n"},
		{name: "unavailable events"},
	} {
		t.Run(test.name, func(t *testing.T) {
			group := &sandboxCgroup{directory: t.TempDir()}
			if test.events != "" {
				if err := os.WriteFile(filepath.Join(group.directory, "memory.events"), []byte(test.events), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := group.memoryError(); (err == nil) != test.valid {
				t.Fatalf("memory observation error = %v, want valid = %v", err, test.valid)
			}
		})
	}
}

func TestSandboxCgroupAvailability(t *testing.T) {
	group, err := newSandboxCgroup()
	if err != nil {
		target, scratch := t.TempDir(), t.TempDir()
		if sandbox, err := NewSandbox(target, scratch); err == nil || sandbox != nil {
			t.Fatal("sandbox accepted unavailable aggregate memory enforcement")
		}
		sandbox := &linuxSandbox{bwrap: "/bin/true", prlimit: "/bin/true", target: target, scratch: scratch}
		if _, err := sandbox.Execute(t.Context(), []string{"fixture"}, scratch); err == nil {
			t.Fatal("sandbox executed without an aggregate memory controller")
		}
		t.Log("aggregate memory controller unavailable; constructor and execution both refused")
		return
	}
	command := exec.CommandContext(t.Context(), "/bin/sh", "-c", `cat /proc/self/cgroup; /bin/sh -c 'cat /proc/self/cgroup'`)
	var output strings.Builder
	command.Stdout = &output
	if err := runSandboxCgroupCommand(command, group); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 || lines[0] != lines[1] || !strings.HasSuffix(lines[0], "/"+filepath.Base(group.directory)) {
		t.Fatalf("fixture descendants did not inherit the bounded cgroup: %q", output.String())
	}
	if _, err := os.Stat(group.directory); !os.IsNotExist(err) {
		t.Fatalf("fixture cgroup was not removed: %v", err)
	}
}
