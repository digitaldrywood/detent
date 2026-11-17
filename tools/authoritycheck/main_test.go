package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthorityGrowth(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = root
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Example", "GIT_AUTHOR_EMAIL=test@example.test", "GIT_COMMITTER_NAME=Example", "GIT_COMMITTER_EMAIL=test@example.test"}
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", args[0], err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, contents string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func() string {
		t.Helper()
		git("add", ".")
		git("-c", "commit.gpgsign=false", "commit", "-qm", "fixture")
		return git("rev-parse", "HEAD")
	}
	git("init", "-q")
	const registry = "internal/invariants/authority_operations.txt"
	const subset = "internal/runnerauth LoadRoutingCache\n"
	write("go.mod", "module example.test\n")
	initial := commit()
	write(registry, subset)
	partial := commit()
	if err := check(t.Context(), root, initial, partial); err == nil {
		t.Fatal("partial first registry passed")
	}
	actual, err := os.ReadFile("../../" + registry)
	if err != nil {
		t.Fatal(err)
	}
	operations := string(actual)
	write(registry, operations)
	complete := commit()
	if err := check(t.Context(), root, initial, complete); err != nil {
		t.Fatalf("complete first registry: %v", err)
	}
	const file = "internal/example/client.go"
	const header = "package example\nimport cache \"github.com/digitaldrywood/detent/internal/runnerauth\"\n"
	const call = "func old() { _, _ = cache.LoadRoutingCache(\"path\") }\n"
	write(file, header+call)
	base := commit()
	write(file, header+"func fresh() { _, _ = cache.LoadRoutingCache(\"path\") }\n")
	write(registry, operations+"internal/runnerauth LatestRoutingCache\n")
	head := commit()
	if err := check(t.Context(), root, base, head); err == nil || !strings.Contains(err.Error(), "fresh") {
		t.Fatalf("new caller under an expanded candidate baseline: %v", err)
	}
	write(registry, strings.Replace(operations, subset, "", 1))
	head = commit()
	if err := check(t.Context(), root, base, head); err == nil || !strings.Contains(err.Error(), "removed audited operation internal/runnerauth LoadRoutingCache") {
		t.Fatalf("candidate removed audited operation: %v", err)
	}
	write(registry, operations)
	write(file, "package example\n")
	head = commit()
	if err := check(t.Context(), root, base, head); err != nil {
		t.Fatal(err)
	}
	const updateFile = "internal/update/use.go"
	const types = "package update\ntype Scheduler struct{}\ntype Other struct{}\n"
	write(updateFile, types+"func (s *Other) check() { s.recordEnrolledApplied(); loadSchedulerState := func() {}; loadSchedulerState() }\nfunc shadow() { type Scheduler = Other; var s Scheduler; s.recordEnrolledApplied() }\n")
	clean := commit()
	if err := check(t.Context(), root, base, clean); err != nil {
		t.Fatalf("unrelated method or shadowed function: %v", err)
	}
	for _, receiver := range []string{"s", "(*s)", "(&(*s))"} {
		t.Run(receiver, func(t *testing.T) {
			write(updateFile, types+"func (s *Scheduler) check() { "+receiver+".recordEnrolledApplied() }\n")
			head := commit()
			if err := check(t.Context(), root, clean, head); err == nil || !strings.Contains(err.Error(), "Scheduler.recordEnrolledApplied") {
				t.Fatalf("new audited method call on %s: %v", receiver, err)
			}
		})
	}
	const definitions = types + "func (*Scheduler) recordEnrolledApplied() {}\nfunc (*Other) recordEnrolledApplied() {}\n"
	const aliasFile = "internal/update/alias.go"
	const aliases = "type Value = Scheduler\ntype Ptr = *Value\ntype Receiver = Ptr\n"
	const useAlias = "func check(s Receiver) { s.recordEnrolledApplied() }\n"
	for _, tt := range []struct {
		name    string
		sibling bool
		target  string
		want    bool
	}{
		{"same file scheduler", false, "Scheduler", true},
		{"same file other", false, "Other", false},
		{"sibling scheduler", true, "Scheduler", true},
		{"sibling other", true, "Other", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			decls := strings.Replace(aliases, "Scheduler", tt.target, 1)
			source, sibling := definitions, "package update\n"
			if tt.sibling {
				sibling += decls
			} else {
				source += decls
			}
			write(aliasFile, sibling)
			write(updateFile, source+"func check(s Receiver) {}\n")
			target := commit()
			write(updateFile, source+useAlias)
			candidate := commit()
			write(aliasFile, "invalid working copy")
			err := check(t.Context(), root, target, candidate)
			if tt.want {
				if err == nil || !strings.Contains(err.Error(), "check calls update.Scheduler.recordEnrolledApplied grew from 0 to 1") {
					t.Fatalf("audited alias: %v", err)
				}
			} else if err != nil {
				t.Fatalf("unrelated alias: %v", err)
			}
		})
	}
	t.Run("sibling changes receiver identity", func(t *testing.T) {
		write(aliasFile, "package update\ntype Receiver = *Other\n")
		write(updateFile, definitions+useAlias)
		target := commit()
		write(aliasFile, "package update\ntype Receiver = *Scheduler\n")
		write(updateFile, definitions+"\n"+useAlias)
		candidate := commit()
		write(aliasFile, "package update\ntype Receiver = *Other\n")
		err := check(t.Context(), root, target, candidate)
		if err == nil || !strings.Contains(err.Error(), "check calls update.Scheduler.recordEnrolledApplied grew from 0 to 1") {
			t.Fatalf("receiver identity from each tree: %v", err)
		}
	})
	t.Run("replaced candidate blob", func(t *testing.T) {
		write(updateFile, definitions+"func check(s *Scheduler) {}\n")
		target := commit()
		write(updateFile, definitions+"func check(s *Scheduler) { s.recordEnrolledApplied() }\n")
		candidate := commit()
		original := git("rev-parse", candidate+":"+updateFile)
		replacement := git("rev-parse", target+":"+updateFile)
		git("replace", original, replacement)
		if git("cat-file", "blob", original) != git("cat-file", "blob", replacement) {
			t.Fatal("replacement did not mask candidate blob")
		}
		err := check(t.Context(), root, target, candidate)
		if err == nil || !strings.Contains(err.Error(), "check calls update.Scheduler.recordEnrolledApplied grew from 0 to 1") {
			t.Fatalf("growth masked by a replacement ref: %v", err)
		}
	})
	t.Run("cancel during last blob analysis", func(t *testing.T) {
		write(file, header+call)
		target := commit()
		write(file, header+call+"\n")
		candidate := commit()
		lastBlob := git("rev-parse", target+":"+file)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		read := func(args ...string) ([]byte, error) {
			out, err := readObjects(ctx, root, args...)
			if len(args) == 3 && args[0] == "cat-file" && args[2] == lastBlob && err == nil {
				cancel()
			}
			return out, err
		}
		if err := checkCommits(ctx, read, target, candidate); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled with equal counts in the final blob: %v", err)
		}
	})
}
