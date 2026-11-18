package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAuthorityGrowth(t *testing.T) {
	root := t.TempDir()
	gitInput := func(input string, args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = root
		command.Stdin = strings.NewReader(input)
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Example", "GIT_AUTHOR_EMAIL=test@example.test", "GIT_COMMITTER_NAME=Example", "GIT_COMMITTER_EMAIL=test@example.test"}
		out, err := command.Output()
		if err != nil {
			t.Fatalf("git %s: %v", args[0], err)
		}
		return strings.TrimSpace(string(out))
	}
	git := func(args ...string) string {
		t.Helper()
		return gitInput("", args...)
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
	link := func(name, target string) {
		t.Helper()
		file := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, file); err != nil {
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
	write("go.mod", "module "+strings.TrimSuffix(module, "/")+"\n\ngo 1.26\n")
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
	write("internal/runnerauth/cache.go", "package runnerauth\nfunc LoadRoutingCache(string) (int, error) { return 0, nil }\n")
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
	if err := check(t.Context(), root, base, head); err == nil || !strings.Contains(err.Error(), "removed audited operation") || !strings.Contains(err.Error(), "internal/runnerauth") || !strings.HasSuffix(err.Error(), " LoadRoutingCache") {
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
	const definitions = types + "func (*Scheduler) recordEnrolledApplied() int { return 1 }\nfunc (*Other) recordEnrolledApplied() int { return 1 }\n"
	write(updateFile, definitions+"func (s *Other) check() { s.recordEnrolledApplied(); loadSchedulerState := func() {}; loadSchedulerState() }\nfunc shadow() { type Scheduler = Other; var s Scheduler; s.recordEnrolledApplied() }\n")
	clean := commit()
	if err := check(t.Context(), root, base, clean); err != nil {
		t.Fatalf("unrelated method or shadowed function: %v", err)
	}
	for _, receiver := range []string{"s", "(*s)", "(&(*s))"} {
		t.Run(receiver, func(t *testing.T) {
			write(updateFile, definitions+"func (s *Scheduler) check() { "+receiver+".recordEnrolledApplied() }\n")
			head := commit()
			if err := check(t.Context(), root, clean, head); err == nil || !strings.Contains(err.Error(), "Scheduler.recordEnrolledApplied") {
				t.Fatalf("new audited method call on %s: %v", receiver, err)
			}
		})
	}
	const aliasFile = "internal/update/alias.go"
	const aliases = "type Value = Scheduler\ntype Ptr = *Value\ntype Receiver = Ptr\n"
	const useAlias = "func check(s Receiver) { s.recordEnrolledApplied() }\n"
	for _, tt := range []struct {
		name         string
		sibling      bool
		declarations string
		want         bool
	}{
		{"same file scheduler", false, aliases, true},
		{"same file other", false, strings.Replace(aliases, "Scheduler", "Other", 1), false},
		{"sibling scheduler", true, aliases, true},
		{"sibling other", true, strings.Replace(aliases, "Scheduler", "Other", 1), false},
		{"promoted scheduler", true, "type Receiver struct { *Scheduler }\n", true},
		{"promoted other", true, "type Receiver struct { *Other }\n", false},
		{"override on embedding type", true, "type Receiver struct { *Scheduler }\nfunc (Receiver) recordEnrolledApplied() {}\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source, sibling := definitions, "package update\n"
			if tt.sibling {
				sibling += tt.declarations
			} else {
				source += tt.declarations
			}
			write(aliasFile, sibling)
			write(updateFile, source+"func check(s Receiver) {}\n")
			target := commit()
			write(updateFile, source+useAlias)
			candidate := commit()
			write(aliasFile, "invalid working copy")
			err := check(t.Context(), root, target, candidate)
			if tt.want {
				if err == nil || !strings.Contains(err.Error(), "check calls Scheduler.recordEnrolledApplied in \""+module+"internal/update\" grew from 0 to 1") {
					t.Fatalf("registered method: %v", err)
				}
			} else if err != nil {
				t.Fatalf("unrelated method: %v", err)
			}
		})
	}
	t.Run("method selected through transitive aliases", func(t *testing.T) {
		const owner = "internal/record"
		const client = "internal/probe/use.go"
		const header = "package probe\nimport \"" + module + "internal/bridge\"\n"
		write(registry, operations+owner+" State.Record\n")
		write(owner+"/state.go", "package record\ntype State struct{}\nfunc (*State) Record() {}\n")
		write("internal/carrier/types.go", "package carrier\nimport \""+module+owner+"\"\ntype Alias = record.State\ntype Wrapped struct { *Alias }\n")
		write("internal/bridge/types.go", "package bridge\nimport \""+module+"internal/carrier\"\ntype Alias = carrier.Wrapped\ntype Other struct { Alias }\nfunc (Other) Record() {}\n")
		write(client, header+"func run(v bridge.Other) { v.Record() }\n")
		target := commit()
		write(client, header+"func run(v bridge.Alias) { v.Record() }\n")
		candidate := commit()
		err := check(t.Context(), root, target, candidate)
		if err == nil || !strings.Contains(err.Error(), "run calls State.Record in \""+module+owner+"\" grew from 0 to 1") {
			t.Errorf("promoted method: %v", err)
		}
		if err := check(t.Context(), root, candidate, target); err != nil {
			t.Errorf("overriding method: %v", err)
		}
	})
	t.Run("unchanged caller through platform dependency", func(t *testing.T) {
		const owner = "internal/platformrecord"
		const client = "internal/platformclient/use.go"
		const typesFile = "internal/platformtype/type.go"
		const typesHeader = "package platformtype\nimport \"" + module + owner + "\"\n"
		write(registry, operations+owner+" State.Record\n")
		write(owner+"/state.go", "package platformrecord\ntype State struct{}\nfunc (State) Record() {}\ntype Other struct{}\nfunc (Other) Record() {}\n")
		write("internal/platformbridge/value.go", "package platformbridge\nimport \""+module+"internal/platformtype\"\ntype Value = platformtype.Value\n")
		write(client, "package platformclient\nimport \""+module+"internal/platformbridge\"\nfunc run(v platformbridge.Value) { v.Record() }\n")
		for _, osName := range []string{"windows", "linux", "darwin"} {
			t.Run(osName, func(t *testing.T) {
				write(typesFile, "//go:build !"+osName+"\n\n"+typesHeader+"type Value = platformrecord.Other\n")
				override := "internal/platformtype/type_" + osName + ".go"
				write(override, typesHeader+"type Value = platformrecord.Other\n")
				target := commit()
				write(override, typesHeader+"type Value = platformrecord.State\n")
				candidate := commit()
				want := strconv.Quote(client) + ": run calls State.Record in \"" + module + owner + "\" grew from 0 to 1"
				if err := check(t.Context(), root, target, candidate); err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("platform growth: %v", err)
				}
				if err := check(t.Context(), root, candidate, target); err != nil {
					t.Errorf("platform decrease: %v", err)
				}
				write(override, "//go:build ignore\n\n"+typesHeader+"type Value = platformrecord.State\n")
			})
		}
	})

	t.Run("platform specific caller", func(t *testing.T) {
		t.Setenv("GOEXPERIMENT", "loopvar")
		const owner = "internal/platformrecord"
		const client = "internal/platformprobe/use_windows.go"
		const header = "//go:build !goexperiment.loopvar\n\npackage platformprobe\nimport r \"" + module + owner + "\"\nimport \"syscall\"\nvar _ syscall.Handle\n"
		write(registry, operations+owner+" State.Record\n")
		write(owner+"/state.go", "package platformrecord\ntype State struct{}\nfunc (State) Record() {}\ntype Other struct{}\nfunc (Other) Record() {}\n")
		const linuxFile = "internal/platformprobe/unselected_linux.go"
		write(linuxFile, "package platformprobe\nfunc incomplete(\n")
		write(client, header+"func run(r.Other) {}\n")
		target := commit()
		for _, tc := range []struct {
			typ    string
			growth bool
		}{{"Other", false}, {"State", true}} {
			write(client, header+"func run(v r."+tc.typ+") { v.Record() }\n")
			candidate := commit()
			err := check(t.Context(), root, target, candidate)
			if tc.growth {
				if err == nil || !strings.Contains(err.Error(), "run calls State.Record") {
					t.Errorf("Windows registered method: %v", err)
				}
			} else if err != nil {
				t.Errorf("Windows unrelated method: %v", err)
			}
		}
		write(linuxFile, "package platformprobe\n")
	})

	t.Run("growth moves between release architectures", func(t *testing.T) {
		const owner = "internal/archrecord"
		const client = "internal/archclient/use.go"
		const prefix = "package archclient\nimport r \"" + module + owner + "\"\n"
		write(registry, operations+owner+" State.Record\n")
		write(owner+"/state.go", "package archrecord\ntype State struct{}\nfunc (State) Record() {}\ntype Other struct{}\nfunc (Other) Record() {}\n")
		write(client, "package archclient\nfunc run(v Value) { v.Record() }\n")
		setup := func(amd64, arm64 string) string {
			write("internal/archclient/type_amd64.go", prefix+"type Value = r."+amd64+"\n")
			write("internal/archclient/type_arm64.go", prefix+"type Value = r."+arm64+"\n")
			return commit()
		}
		target := setup("State", "Other")
		candidate := setup("Other", "State")
		for _, refs := range [][2]string{{target, candidate}, {candidate, target}} {
			if err := check(t.Context(), root, refs[0], refs[1]); err == nil || !strings.Contains(err.Error(), strconv.Quote(client)+": run calls State.Record") {
				t.Errorf("growth on one architecture: %v", err)
			}
		}
	})

	t.Run("unchanged caller of a newly direct function", func(t *testing.T) {
		const owner = "internal/functionowner"
		const client = "cmd/functionclient/use.go"
		const declaration = owner + "/record.go"
		write(registry, operations+owner+" Record\n")
		write(client, "package main\nimport . \""+module+owner+"\"\nfunc run() { Record() }\n")
		write(declaration, "package functionowner\nvar Record = func() {}\n")
		target := commit()
		write(declaration, "package functionowner\nfunc Record() {}\n")
		candidate := commit()
		if err := check(t.Context(), root, target, candidate); err == nil || !strings.Contains(err.Error(), strconv.Quote(client)+": run calls Record") {
			t.Errorf("function replaced indirect call: %v", err)
		}
		if err := check(t.Context(), root, candidate, target); err != nil {
			t.Errorf("function replaced by variable: %v", err)
		}
	})

	t.Run("shallower field in imported embedding", func(t *testing.T) {
		const use = "internal/update/selection.go"
		const header = "package update\nimport \"" + module + "internal/view\"\n"
		const body = "type deep struct { Target *Other }\ntype fallback struct { deep }\nfunc inspect(v struct { view.Fields; fallback }) { v.Target.recordEnrolledApplied() }\n"
		write(updateFile, definitions)
		write(aliasFile, "package update\n")
		write("internal/fields/box.go", "package fields\ntype Box[T any] struct { Target T }\n")
		const bridge = "package view\nimport \"" + module + "internal/fields\"\ntype Fields[T any] = fields.Box[T]\n"
		write("internal/view/fields.go", bridge)
		write(use, header+strings.Replace(body, "view.Fields", "view.Fields[*Other]", 1))
		target := commit()
		write(use, header+strings.Replace(body, "view.Fields", "view.Fields[*Scheduler]", 1))
		candidate := commit()
		err := check(t.Context(), root, target, candidate)
		if err == nil || !strings.Contains(err.Error(), "inspect calls Scheduler.recordEnrolledApplied") {
			t.Errorf("selected field: %v", err)
		}
		if err := check(t.Context(), root, candidate, target); err != nil {
			t.Errorf("unrelated receiver: %v", err)
		}
		write(use, "package update\n")
	})
	t.Run("sibling changes receiver identity", func(t *testing.T) {
		write(aliasFile, "package update\ntype Receiver = *Other\n")
		write(updateFile, definitions+useAlias)
		target := commit()
		write(aliasFile, "package update\ntype Receiver = *Scheduler\n")
		candidate := commit()
		write(aliasFile, "package update\ntype Receiver = *Other\n")
		err := check(t.Context(), root, target, candidate)
		if err == nil || !strings.Contains(err.Error(), "check calls Scheduler.recordEnrolledApplied in \""+module+"internal/update\" grew from 0 to 1") {
			t.Fatalf("receiver identity from each tree: %v", err)
		}
	})
	t.Run("unrelated body keeps the fast path", func(t *testing.T) {
		const other = "func bodyOnly() int { return 1 }\n"
		write(aliasFile, "package update\ntype Receiver = *Scheduler\n")
		write(updateFile, definitions+useAlias+other)
		target := commit()
		write(updateFile, definitions+useAlias+strings.Replace(other, "return 1", "return 2", 1))
		candidate := commit()
		modBlob := git("rev-parse", candidate+":go.mod")
		read := func(input string, args ...string) ([]byte, error) {
			if args[0] == "cat-file" && (input != "" || args[len(args)-1] == modBlob) {
				t.Fatal("unrelated body triggered package analysis")
			}
			return readObjects(t.Context(), root, input, args...)
		}
		if err := checkCommits(t.Context(), read, target, candidate); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("method calls with generic dependency", func(t *testing.T) {
		const client = "internal/update/unresolved.go"
		const header = "package update\nimport \"" + module + "internal/other\"\n"
		write("internal/other/update.go", "package other\nfunc Value[T any](_ string, v T) T { return v }\n")
		write(client, "package update\n")
		target := commit()
		write(client, header+"type Embedded struct { *Scheduler }\nfunc checkState(s Embedded) { _ = map[int]int{other.Value(\"key\", 1): s.recordEnrolledApplied()} }\n")
		promoted := commit()
		if err := check(t.Context(), root, target, promoted); err == nil || !strings.Contains(err.Error(), "checkState calls Scheduler.recordEnrolledApplied in \""+module+"internal/update\" grew from 0 to 1") {
			t.Fatalf("skipped promoted method call: %v", err)
		}
		write(client, header+"//line placeholder-source:81\nfunc checkState(s *Scheduler) { other.Value(\"placeholder literal\", s).recordEnrolledApplied() }\n")
		candidate := commit()
		if err := check(t.Context(), root, target, candidate); err == nil || !strings.Contains(err.Error(), "checkState calls Scheduler.recordEnrolledApplied") {
			t.Fatalf("inferred receiver: %v", err)
		}
		write(client, header+"//line placeholder-source:81\nfunc checkState() { other.Missing().recordEnrolledApplied() }\n")
		indeterminate := commit()
		err := check(t.Context(), root, target, indeterminate)
		write(client, "package update\n")
		if err == nil || err.Error() != "candidate: "+strconv.Quote(client)+":4: unresolved possible authority call" {
			t.Fatalf("unresolved method diagnostic: %v", err)
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
		t.Setenv("GIT_DIR", t.TempDir())
		err := check(t.Context(), root, target, candidate)
		if err == nil || !strings.Contains(err.Error(), "check calls Scheduler.recordEnrolledApplied in \""+module+"internal/update\" grew from 0 to 1") {
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
		read := func(input string, args ...string) ([]byte, error) {
			out, err := readObjects(ctx, root, input, args...)
			if len(args) == 3 && args[0] == "cat-file" && args[2] == lastBlob && err == nil {
				cancel()
			}
			return out, err
		}
		if err := checkCommits(ctx, read, target, candidate); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled with equal counts in the final blob: %v", err)
		}
	})
	t.Run("parse error reports only committed location and category", func(t *testing.T) {
		write(file, "package example\n//line placeholder-source:81\n\"placeholder literal\"\n")
		candidate := commit()
		err := check(t.Context(), root, base, candidate)
		if err == nil || err.Error() != "candidate: "+strconv.Quote(file)+":3: Go syntax error" {
			t.Errorf("unexpected parse diagnostic: %v", err)
		}
	})
	t.Run("caller labels", func(t *testing.T) {
		const callerFile = "internal/labels/caller.go"
		prior := git("rev-parse", "HEAD")
		for _, tc := range []struct{ receiver, label string }{
			{"*Example[A, B]", "Example.run"},
			{"struct { Value string `fixture label` }", ""},
		} {
			write(callerFile, header+"type Example[A, B any] struct{}\nfunc ("+tc.receiver+") run() { cache.LoadRoutingCache(\"path\") }\n")
			candidate := commit()
			err := check(t.Context(), root, prior, candidate)
			want := "INV-24: new client state authority sites:\n" + strconv.Quote(callerFile) + ": " + tc.label + " calls LoadRoutingCache in \"" + module + "internal/runnerauth\" grew from 0 to 1"
			if tc.label == "" {
				want = "candidate: " + strconv.Quote(callerFile) + ":4: unresolved possible authority call"
			}
			if err == nil || err.Error() != want {
				t.Errorf("caller diagnostic: got %v, want %s", err, want)
			}
		}
	})
	t.Run("linked receiver declaration", func(t *testing.T) {
		const declaration = "fixtures/receiver.src"
		const caller = "internal/update/linked_use.go"
		write(updateFile, definitions)
		write(declaration, "package update\ntype Linked struct { *Scheduler }\n")
		link("internal/update/linked_type.go", "../../"+declaration)
		write(caller, "package update\nfunc inspect(s Linked) {}\n")
		target := commit()
		write(caller, "package update\nfunc inspect(s Linked) { s.recordEnrolledApplied() }\n")
		candidate := commit()
		write(declaration, "package update\ntype Linked struct { *Other }\n")
		err := check(t.Context(), root, target, candidate)
		if err == nil || !strings.Contains(err.Error(), strconv.Quote(caller)+": inspect calls Scheduler.recordEnrolledApplied in \""+module+"internal/update\" grew from 0 to 1") {
			t.Errorf("promoted method from committed linked declaration: %v", err)
		}
	})
	t.Run("same caller switches registered package", func(t *testing.T) {
		const first = "internal/first/example"
		const second = "internal/second/example"
		const client = "cmd/client/main.go"
		caller := func(pkg string) string {
			return "package main\nimport \"" + module + pkg + "\"\nfunc run() { example.Load() }\n"
		}
		write(first+"/load.go", "package example\nfunc Load() {}\n")
		write(second+"/load.go", "package example\nfunc Load() {}\n")
		write(registry, operations+first+" Load\n")
		write(client, caller(first))
		target := commit()
		write(registry, operations+first+" Load\n"+second+" Load\n")
		write(client, caller(second))
		candidate := commit()
		err := check(t.Context(), root, target, candidate)
		want := "INV-24: new client state authority sites:\n" + strconv.Quote(client) + ": run calls Load in \"" + module + second + "\" grew from 0 to 1"
		if err == nil || err.Error() != want {
			t.Fatalf("switched package: got %v, want %s", err, want)
		}
	})
	t.Run("registered function calls", func(t *testing.T) {
		const owner = "internal/example/v2"
		const client = "cmd/example/main.go"
		const importPath = "\"" + module + owner + "\""
		write(registry, operations+owner+" Read\n")
		write(owner+"/read.go", "package archive\nfunc Read[A, B any](a A, b B) int { return 1 }\n")
		write("internal/other/archive/use.go", "package other\nfunc Use() int { return 1 }\n")
		write("internal/other/archive/reader.go", "package other\ntype Reader struct{}\nfunc (Reader) Read() int { return 1 }\n")
		for _, tc := range []struct {
			name, imports, before, after string
			growth                       bool
		}{
			{"declared name", importPath, "_ = archive.Read[int, int]", "archive.Read(1, 2)", true},
			{"colliding import basename", "(\"" + module + "internal/other/archive\"; " + importPath + ")", "other.Use(); _ = archive.Read[int, int]", "other.Use(); archive.Read(1, 2)", true},
			{"map value after unresolved key", "(\"" + module + "internal/other/archive\"; " + importPath + ")", "_ = map[int]int{other.Use(): 0}; _ = archive.Read[int, int]", "_ = map[int]int{other.Use(): archive.Read(1, 2)}", true},
			{"shadowed map value after unresolved key", "(\"" + module + "internal/other/archive\"; . " + importPath + ")", "_ = Read[int, int]; _ = other.Use()", "_ = Read[int, int]; Read := func() int { return 1 }; _ = map[int]int{other.Use(): Read()}", false},
			{"shadowed qualifier with unresolved type", "(\"" + module + "internal/other/archive\"; a " + importPath + ")", "_ = a.Read[int, int]; _ = other.Reader{}", "_ = a.Read[int, int]; a := other.Reader{}; a.Read()", false},
			{"one type argument", "a " + importPath, "_ = a.Read[int, int]", "a.Read[int](1, 2)", true},
			{"multiple type arguments", ". " + importPath, "_ = Read[int, int]", "(Read[int, int])(1, 2)", true},
			{"shadowed indexed function", "a " + importPath, "a.Read(1, 2)", "a.Read(1, 2); a := struct{ Read []func() }{[]func(){func(){}}}; a.Read[0]()", false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				src := "package main\nimport " + tc.imports + "\nfunc run() { "
				write(client, src+tc.before+" }\n")
				target := commit()
				write(client, src+tc.after+" }\n")
				candidate := commit()
				err := check(t.Context(), root, target, candidate)
				if !tc.growth {
					if err != nil {
						t.Fatal("counted an indirect call through a shadowed name")
					}
				} else if err == nil || !strings.Contains(err.Error(), "run calls Read in \""+module+owner+"\" grew from 0 to 1") {
					t.Fatal("missing direct call growth")
				}
			})
		}
		t.Run("package renamed between commits", func(t *testing.T) {
			const src = "package main\nimport " + importPath + "\nfunc run() { "
			write(client, src+"archive.Read(1, 2) }\n")
			target := commit()
			write(owner+"/read.go", "package history\nfunc Read[A, B any](a A, b B) {}\n")
			write(client, src+"history.Read(1, 2); history.Read(3, 4) }\n")
			candidate := commit()
			write(owner+"/read.go", "package uncommitted\n")
			err := check(t.Context(), root, target, candidate)
			if err == nil || !strings.Contains(err.Error(), "run calls Read in \""+module+owner+"\" grew from 1 to 2") {
				t.Fatal("expected one call in the target and two in the candidate")
			}
		})
		t.Run("indexed sibling variable", func(t *testing.T) {
			write(client, "package main\n")
			write(owner+"/read.go", "package archive\nvar Read = []func(){func(){}}\n")
			write(owner+"/use.go", "package archive\nfunc run() {}\n")
			target := commit()
			write(owner+"/use.go", "package archive\nfunc run() { Read[0]() }\n")
			candidate := commit()
			if err := check(t.Context(), root, target, candidate); err != nil {
				t.Fatal("counted an indirect call through a sibling variable")
			}
		})
	})
	t.Run("independent init declarations", func(t *testing.T) {
		const invoke = "cache.LoadRoutingCache(\"path\")"
		write(file, header+"func init() { "+invoke+" }\nfunc init() {}\n")
		target := commit()
		write(file, header+"func init() {}\nfunc init() { "+invoke+" }\n")
		candidate := commit()
		err := check(t.Context(), root, target, candidate)
		if err == nil || !strings.Contains(err.Error(), "init#2 calls LoadRoutingCache") {
			t.Fatal("moving a call to the second init masked its growth")
		}
	})
	t.Run("escaped committed path", func(t *testing.T) {
		const odd = "internal/example/quoted \"line\n\x1b[2K.go"
		target := git("rev-parse", "HEAD")
		withPath := func(mode, contents string) string {
			t.Helper()
			git("read-tree", target)
			blob := gitInput(contents, "hash-object", "-w", "--stdin")
			gitInput(mode+" "+blob+"\t"+odd+"\x00", "update-index", "-z", "--index-info")
			return git("commit-tree", git("write-tree"), "-p", target, "-m", "fixture")
		}
		candidate := withPath("100644", header+call)
		err := check(t.Context(), root, target, candidate)
		if err == nil || err.Error() != "INV-24: new client state authority sites:\n"+strconv.Quote(odd)+": old calls LoadRoutingCache in \""+module+"internal/runnerauth\" grew from 0 to 1" {
			t.Error("growth diagnostic did not safely render the committed path")
		}
		candidate = withPath("100644", "package example\n//line private-source:81\n\"private literal\"\n")
		err = check(t.Context(), root, target, candidate)
		if err == nil || err.Error() != "candidate: "+strconv.Quote(odd)+":3: Go syntax error" {
			t.Error("parse diagnostic did not safely render only the committed location and category")
		}
		candidate = withPath("120000", "absent")
		err = check(t.Context(), root, target, candidate)
		if err == nil || err.Error() != "candidate: "+strconv.Quote(odd)+": Go symlink must resolve to a committed file in the same tree" {
			t.Error("symlink diagnostic did not safely render the committed path")
		}
		git("read-tree", target)
	})
	t.Run("committed production link", func(t *testing.T) {
		const linked = "internal/config/linked.go"
		const contents = "fixtures/client.src"
		const source = "package config\nfunc inspect() { "
		const operation = "nativeMachinePrompt()"
		for _, excluded := range []string{"internal/example/client_test.go", "cmd/testdata/client.go", "tools/_example.go", "tools/.example.go", "docs/example.go", "internal/example/notes.txt"} {
			link(excluded, "untracked-fixture")
		}
		write("internal/config/prompt.go", "package config\nfunc nativeMachinePrompt() {}\n")
		prior := commit()
		write(contents, source+operation+" }\n")
		link(linked, "../../"+contents)
		target := commit()
		checkGrowth := func(base, head, counts string) {
			t.Helper()
			err := check(t.Context(), root, base, head)
			want := "INV-24: new client state authority sites:\n" + strconv.Quote(linked) + ": inspect calls nativeMachinePrompt in \"" + module + "internal/config\" grew from " + counts
			if err == nil || err.Error() != want {
				t.Errorf("linked caller: got %v, want %s", err, want)
			}
		}
		checkGrowth(prior, target, "0 to 1")
		write(contents, source+operation+"; "+operation+" }\n")
		candidate := commit()
		git("replace", git("rev-parse", candidate+":"+contents), git("rev-parse", target+":"+contents))
		write(contents, "uncommitted fixture")
		checkGrowth(target, candidate, "1 to 2")
		if err := check(t.Context(), root, candidate, target); err != nil {
			t.Errorf("decreased linked call count: %v", err)
		}
		git("mv", contents, "fixtures/renamed.src")
		missing := commit()
		for _, tc := range []struct{ base, head, side string }{
			{target, missing, "candidate"},
			{missing, target, "target"},
		} {
			err := check(t.Context(), root, tc.base, tc.head)
			if err == nil || err.Error() != tc.side+": "+strconv.Quote(linked)+": Go symlink must resolve to a committed file in the same tree" {
				t.Errorf("unresolved %s link: %v", tc.side, err)
			}
		}
		if err := check(t.Context(), root, missing, prior); err != nil {
			t.Errorf("removed link: %v", err)
		}
	})
}
