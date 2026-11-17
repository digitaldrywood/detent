package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"slices"
	"strconv"
	"strings"
)

const module = "github.com/digitaldrywood/detent/"

const registryPath = "internal/invariants/authority_operations.txt"

type operations map[string][]string

type readGit func(...string) ([]byte, error)

func registry(read readGit, revision string) (operations, error) {
	entry, err := read("ls-tree", revision, "--", registryPath)
	if err != nil {
		return nil, err
	}
	if len(entry) == 0 {
		return operations{}, nil
	}
	fields := strings.Fields(string(entry))
	if len(fields) != 4 || fields[0] != "100644" || fields[1] != "blob" {
		return nil, errors.New("invalid authority operations file")
	}
	raw, err := read("cat-file", "blob", fields[2])
	if err != nil {
		return nil, err
	}
	ops := operations{}
	for line := range strings.Lines(string(raw)) {
		parts := strings.Fields(line)
		if len(parts) != 2 || !strings.HasPrefix(parts[0], "internal/") || path.Clean(parts[0]) != parts[0] {
			return nil, errors.New("invalid audited operation")
		}
		receiver, method, qualified := strings.Cut(parts[1], ".")
		if !token.IsIdentifier(receiver) || qualified && !token.IsIdentifier(method) {
			return nil, errors.New("invalid audited operation name")
		}
		owner := module + parts[0]
		if slices.Contains(ops[owner], parts[1]) {
			return nil, errors.New("duplicate audited operation")
		}
		ops[owner] = append(ops[owner], parts[1])
	}
	return ops, nil
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: authoritycheck <base> <head>")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := check(ctx, ".", os.Args[1], os.Args[2])
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func readObjects(ctx context.Context, root string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "--no-replace-objects")
	cmd.Args = append(cmd.Args, args...)
	cmd.Dir = root
	cmd.Env = slices.DeleteFunc(os.Environ(), func(s string) bool { return strings.HasPrefix(s, "GIT_") })
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	return output, nil
}

func check(ctx context.Context, root, base, head string) error {
	return checkCommits(ctx, func(args ...string) ([]byte, error) { return readObjects(ctx, root, args...) }, base, head)
}

func checkCommits(ctx context.Context, read readGit, base, head string) error {
	err := read.compare(ctx, base, head)
	if cancelled := ctx.Err(); cancelled != nil {
		return cancelled
	}
	return err
}

func (read readGit) compare(ctx context.Context, base, head string) error {
	for _, ref := range []string{base, head} {
		if len(ref) != 40 && len(ref) != 64 {
			return errors.New("authority check requires two full commit IDs")
		}
		if _, err := hex.DecodeString(ref); err != nil {
			return fmt.Errorf("invalid commit ID: %w", err)
		}
	}
	audited, err := registry(read, head)
	if err != nil {
		return err
	}
	if len(audited) == 0 {
		return errors.New("candidate lacks authority operations")
	}
	prior, err := registry(read, base)
	if err != nil {
		return err
	}
	if len(prior) == 0 {
		prior = operations{
			module + "internal/config":           {"nativeMachinePrompt"},
			module + "internal/providercapacity": {"Load"},
			module + "internal/runnerauth":       {"LoadRoutingCache", "SaveRoutingCache"},
			module + "internal/update":           {"Scheduler.recordEnrolledApplied", "loadSchedulerState", "saveSchedulerState"},
		}
	}
	for pkg, names := range prior {
		for _, name := range names {
			if !slices.Contains(audited[pkg], name) {
				return fmt.Errorf("removed audited operation %s %s", strings.TrimPrefix(pkg, module), name)
			}
		}
	}
	diff, err := read("diff", "--no-ext-diff", "--raw", "--no-abbrev", "--no-renames", "--diff-filter=AMT", "-z", base, head, "--", "internal", "cmd", "tools")
	if err != nil {
		return err
	}
	entries := strings.Split(strings.TrimSuffix(string(diff), "\x00"), "\x00")
	if len(diff) == 0 {
		return nil
	}
	if len(entries)%2 != 0 {
		return errors.New("invalid git raw diff")
	}
	var problems []string
	target := sourceTree{read: read, revision: base, audited: audited, siblings: make(map[string]sourceFile)}
	candidate := sourceTree{read: read, revision: head, audited: audited, siblings: make(map[string]sourceFile)}
	for i := 0; i < len(entries); i += 2 {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, file := strings.Fields(entries[i]), entries[i+1]
		if len(info) != 5 {
			return errors.New("invalid git diff entry")
		}
		if !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") || strings.Contains(file, "/testdata/") || !strings.HasPrefix(info[1], "100") {
			continue
		}
		after, err := candidate.sites(file, info[3])
		if err != nil {
			return err
		}
		if len(after) == 0 {
			continue
		}
		var before map[string]int
		if strings.HasPrefix(info[0], ":100") {
			before, err = target.sites(file, info[2])
			if err != nil {
				return err
			}
		}
		for site, n := range after {
			if n > before[site] {
				problems = append(problems, fmt.Sprintf("%s: %s grew from %d to %d", file, site, before[site], n))
			}
		}
	}
	if len(problems) == 0 {
		return nil
	}
	slices.Sort(problems)
	return fmt.Errorf("INV-24: new client state authority sites:\n%s", strings.Join(problems, "\n"))
}

type fileImports struct{}

func (fileImports) Import(importPath string) (*types.Package, error) {
	pkg := types.NewPackage(importPath, path.Base(importPath))
	pkg.MarkComplete()
	return pkg, nil
}

type sourceTree struct {
	read     readGit
	revision string
	audited  operations
	siblings map[string]sourceFile
}

type sourceFile struct {
	syntax *ast.File
	group  *sourcePackage
}

type sourcePackage struct {
	set   *token.FileSet
	files []*ast.File
	pkg   *types.Package
	info  types.Info
}

func (p *sourcePackage) resolve(owner string) error {
	if p.pkg != nil {
		return nil
	}
	p.info = types.Info{Uses: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}}
	cfg := types.Config{Importer: fileImports{}, Error: func(error) {}}
	var err error
	p.pkg, err = cfg.Check(owner, p.set, p.files, &p.info)
	if err != nil {
		var partial types.Error
		if !errors.As(err, &partial) {
			return err
		}
	}
	return nil
}

type goBlob struct {
	file string
	id   string
}

func (s *sourceTree) packageBlobs(dir string) ([]goBlob, error) {
	listing, err := s.read("ls-tree", "-z", s.revision+":"+dir)
	if err != nil {
		return nil, err
	}
	var blobs []goBlob
	for record := range strings.SplitSeq(strings.TrimSuffix(string(listing), "\x00"), "\x00") {
		header, name, ok := strings.Cut(record, "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 3 {
			return nil, errors.New("invalid git package tree entry")
		}
		if fields[1] == "blob" && strings.HasPrefix(fields[0], "100") && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			blobs = append(blobs, goBlob{path.Join(dir, name), fields[2]})
		}
	}
	return blobs, nil
}

func (s *sourceTree) load(file, blob string) (sourceFile, error) {
	if source, ok := s.siblings[file]; ok {
		return source, nil
	}
	dir := path.Dir(file)
	withSiblings := slices.ContainsFunc(s.audited[module+dir], func(name string) bool { return strings.Contains(name, ".") })
	blobs := []goBlob{{file, blob}}
	if withSiblings {
		var err error
		blobs, err = s.packageBlobs(dir)
		if err != nil {
			return sourceFile{}, err
		}
	}
	set := token.NewFileSet()
	groups := make(map[string]*sourcePackage)
	var selected sourceFile
	for _, entry := range blobs {
		src, err := s.read("cat-file", "blob", entry.id)
		if err != nil {
			return sourceFile{}, err
		}
		tree, err := parser.ParseFile(set, entry.file, src, parser.SkipObjectResolution)
		if err != nil {
			return sourceFile{}, fmt.Errorf("parse %s: %w", entry.file, err)
		}
		group := groups[tree.Name.Name]
		if group == nil {
			group = &sourcePackage{set: set}
			groups[tree.Name.Name] = group
		}
		group.files = append(group.files, tree)
		source := sourceFile{tree, group}
		if withSiblings {
			s.siblings[entry.file] = source
		}
		if entry.file == file {
			selected = source
		}
	}
	if selected.syntax == nil {
		return sourceFile{}, fmt.Errorf("missing package file %s in %s", file, s.revision)
	}
	return selected, nil
}

func (s *sourceTree) sites(file, blob string) (map[string]int, error) {
	source, err := s.load(file, blob)
	if err != nil {
		return nil, err
	}
	tree, audited := source.syntax, s.audited
	owner := module + path.Dir(file)
	imports := map[string]string{}
	var dot []string
	for _, imp := range tree.Imports {
		pkg, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return nil, fmt.Errorf("import in %s: %w", file, err)
		}
		if len(audited[pkg]) == 0 {
			continue
		}
		name := path.Base(pkg)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "." {
			dot = append(dot, pkg)
		} else if name != "_" {
			imports[name] = pkg
		}
	}
	if len(audited[owner])+len(imports)+len(dot) == 0 {
		return map[string]int{}, nil
	}
	if err := source.group.resolve(owner); err != nil {
		return nil, fmt.Errorf("resolve symbols in %s: %w", file, err)
	}
	info := &source.group.info
	counts := map[string]int{}
	for _, decl := range tree.Decls {
		caller := "package initialization"
		if f, ok := decl.(*ast.FuncDecl); ok {
			caller = f.Name.Name
			if f.Recv != nil {
				var recv bytes.Buffer
				if err := format.Node(&recv, source.group.set, f.Recv.List[0].Type); err != nil {
					return nil, err
				}
				caller = recv.String() + "." + caller
			}
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			pkg, name := "", ""
			switch fun := ast.Unparen(call.Fun).(type) {
			case *ast.Ident:
				name = fun.Name
				obj := info.Uses[fun]
				_, function := obj.(*types.Func)
				if (obj == nil || function) && slices.Contains(audited[owner], name) {
					pkg = owner
				} else if obj == nil {
					for _, candidate := range dot {
						if slices.Contains(audited[candidate], name) {
							pkg = candidate
							break
						}
					}
				}
			case *ast.SelectorExpr:
				name = fun.Sel.Name
				if id, ok := ast.Unparen(fun.X).(*ast.Ident); ok {
					if imported, ok := info.Uses[id].(*types.PkgName); ok {
						pkg = imported.Imported().Path()
					}
				}
				if pkg == "" {
					typ := types.Unalias(info.TypeOf(fun.X))
					if pointer, ok := typ.(*types.Pointer); ok {
						typ = types.Unalias(pointer.Elem())
					}
					if named, ok := typ.(*types.Named); ok && named.Obj().Parent() == source.group.pkg.Scope() {
						pkg, name = owner, named.Obj().Name()+"."+name
					}
				}
			}
			if slices.Contains(audited[pkg], name) {
				counts[caller+" calls "+path.Base(pkg)+"."+name]++
			}
			return true
		})
	}
	return counts, nil
}
