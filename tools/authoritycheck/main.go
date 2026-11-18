package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/printer"
	"go/scanner"
	"go/token"
	"go/types"
	"io"
	"maps"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"
	modpath "golang.org/x/mod/module"
	"golang.org/x/tools/go/packages"

	"github.com/digitaldrywood/detent/internal/toolcache"
	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
)

const module = "github.com/digitaldrywood/detent/"

const registryPath = "internal/invariants/authority_operations.txt"

type operations map[string][]string

type callSite struct {
	caller    string
	pkg       string
	operation string
}

type readGit func(input string, args ...string) ([]byte, error)

func (read readGit) run(args ...string) ([]byte, error) {
	return read("", args...)
}

func registry(read readGit, revision string) (operations, error) {
	entry, err := read.run("ls-tree", revision, "--", registryPath)
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
	raw, err := read.run("cat-file", "blob", fields[2])
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

func readObjects(ctx context.Context, root, input string, args ...string) ([]byte, error) {
	env := slices.DeleteFunc(os.Environ(), func(variable string) bool {
		name, _, _ := strings.Cut(variable, "=")
		return strings.HasPrefix(strings.ToUpper(name), "GIT_")
	})
	output, err := gitexec.OutputWithExactEnv(ctx, root, env, input, append([]string{"--no-replace-objects"}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	return output, nil
}

func check(ctx context.Context, root, base, head string) error {
	return checkCommits(ctx, func(input string, args ...string) ([]byte, error) { return readObjects(ctx, root, input, args...) }, base, head)
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
				return fmt.Errorf("removed audited operation %q %s", strings.TrimPrefix(pkg, module), name)
			}
		}
	}
	target, err := newSourceTree(read, base, audited)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	candidate, err := newSourceTree(read, head, audited)
	if err != nil {
		return fmt.Errorf("candidate: %w", err)
	}
	target.contexts = candidate.contexts
	changed, err := candidate.changes(ctx, target)
	if err != nil {
		return err
	}
	problems := make(map[string]bool)
	paths := slices.Sorted(maps.Keys(changed))
	for i, platform := range platforms {
		bit := platformMask(1 << i)
		var after, before []string
		for _, file := range paths {
			if !productionPath(file) || changed[file]&bit == 0 {
				continue
			}
			after = append(after, file)
		}
		cache := newTypeCache()
		current, err := candidate.countSites(ctx, platform, bit, after, cache)
		if err != nil {
			return fmt.Errorf("candidate: %w", err)
		}
		for file, counts := range current {
			if len(counts) > 0 && target.blobs[file].id != "" {
				before = append(before, file)
			}
		}
		slices.Sort(before)
		if !maps.Equal(target.moduleFiles, candidate.moduleFiles) {
			cache = newTypeCache()
		}
		previous, err := target.countSites(ctx, platform, bit, before, cache)
		if err != nil {
			return fmt.Errorf("target: %w", err)
		}
		for file, counts := range current {
			for site, n := range counts {
				if old := previous[file][site]; n > old {
					problems[fmt.Sprintf("%q: %s calls %s in %q grew from %d to %d", file, site.caller, site.operation, site.pkg, old, n)] = true
				}
			}
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("INV-24: new client state authority sites:\n%s", strings.Join(slices.Sorted(maps.Keys(problems)), "\n"))
}

type platformMask uint8

type buildTarget struct{ os, arch string }

var platforms = [...]buildTarget{
	{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}, {"windows", "amd64"}, {"windows", "arm64"},
}

const allPlatforms platformMask = 1<<len(platforms) - 1

func (p buildTarget) env() []string {
	toolchain, _, _ := strings.Cut(runtime.Version(), "-X:")
	toolchain, _, _ = strings.Cut(toolchain, " X:")
	return append(os.Environ(), "GOMAXPROCS=1", "GOENV=off", "GOWORK=off", "GOFLAGS=", "GOEXPERIMENT=", "GOPACKAGESDRIVER=off", "GOTOOLCHAIN="+toolchain, "GOPROXY=off", "GONOPROXY=none", "GOOS="+p.os, "GOARCH="+p.arch, "GOAMD64=v1", "GOARM64=v8.0", "CGO_ENABLED=0")
}

func (s *sourceTree) buildPlatforms(ctx context.Context, file string, src []byte) (platformMask, error) {
	if mask, known := s.active[file]; known {
		return mask, nil
	}
	var mask platformMask
	for i, platform := range platforms {
		b, ok := s.contexts[platform]
		if !ok {
			tags, err := toolcache.ToolTags(ctx, append(platform.env(), "GO111MODULE=off"))
			if err != nil {
				return 0, fmt.Errorf("load %s/%s Go build context: %w", platform.os, platform.arch, err)
			}
			b = build.Default
			b.GOOS, b.GOARCH, b.CgoEnabled, b.ToolTags = platform.os, platform.arch, false, tags
			s.contexts[platform] = b
		}
		b.OpenFile = func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(src)), nil }
		active, err := b.MatchFile(path.Dir(file), path.Base(file))
		if err != nil {
			return 0, fmt.Errorf("%q: invalid Go build constraint", file)
		}
		if active {
			mask |= 1 << i
		}
	}
	s.active[file] = mask
	return mask, nil
}

type packageImports map[string]*types.Package

func (imports packageImports) Import(importPath string) (*types.Package, error) {
	pkg := imports[importPath]
	if pkg == nil {
		return nil, errors.New("missing Go dependency")
	}
	return pkg, nil
}

type sourceTree struct {
	contexts    map[buildTarget]build.Context
	read        readGit
	revision    string
	audited     operations
	set         *token.FileSet
	blobs       map[string]treeBlob
	packages    map[string][]string
	files       map[string]*ast.File
	active      map[string]platformMask
	moduleFiles map[string]string
}

type treeBlob struct {
	id      string
	symlink bool
}

func newSourceTree(read readGit, revision string, audited operations) (*sourceTree, error) {
	listing, err := read.run("ls-tree", "-r", "-z", revision)
	if err != nil {
		return nil, err
	}
	tree := &sourceTree{
		read: read, revision: revision, audited: audited, contexts: make(map[buildTarget]build.Context),
		set:         token.NewFileSet(),
		files:       make(map[string]*ast.File),
		active:      make(map[string]platformMask),
		moduleFiles: make(map[string]string),
		blobs:       make(map[string]treeBlob),
		packages:    make(map[string][]string),
	}
	for record := range strings.SplitSeq(string(listing), "\x00") {
		if record == "" {
			continue
		}
		header, file, ok := strings.Cut(record, "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 3 {
			return nil, errors.New("invalid git package tree entry")
		}
		name := path.Base(file)
		if name == "go.mod" || file == "go.work" || file == "go.sum" || file == "vendor/modules.txt" {
			if fields[0] == "100644" || fields[0] == "100755" {
				tree.moduleFiles[file] = fields[2]
			} else {
				tree.moduleFiles[file] = ""
			}
		}
		if fields[1] != "blob" || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || strings.Contains(file, "/testdata/") || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		dir := path.Dir(file)
		tree.packages[dir] = append(tree.packages[dir], file)
		tree.blobs[file] = treeBlob{id: fields[2], symlink: fields[0] == "120000"}
	}
	return tree, nil
}

func (s *sourceTree) blobID(file string) (string, error) {
	blob := s.blobs[file]
	if !blob.symlink {
		return blob.id, nil
	}
	out, err := s.read(s.revision+":"+file+"\x00", "cat-file", "--follow-symlinks", "--batch-check=%(objecttype) %(objectname)", "-Z")
	if err != nil {
		return "", fmt.Errorf("%q: resolve committed Go symlink: %w", file, err)
	}
	id, ok := strings.CutPrefix(string(out), "blob ")
	id, terminated := strings.CutSuffix(id, "\x00")
	if !ok || !terminated || len(id) != len(s.revision) {
		return "", fmt.Errorf("%q: Go symlink must resolve to a committed file in the same tree", file)
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", fmt.Errorf("%q: invalid resolved Git blob ID", file)
	}
	s.blobs[file] = treeBlob{id: id}
	return id, nil
}

func productionPath(file string) bool {
	top, _, _ := strings.Cut(file, "/")
	return top == "cmd" || top == "internal" || top == "tools"
}

func (s *sourceTree) parse(ctx context.Context, file string) (*ast.File, error) {
	if f := s.files[file]; f != nil {
		return f, nil
	}
	blob, err := s.blobID(file)
	if err != nil {
		return nil, err
	}
	src, err := s.read.run("cat-file", "blob", blob)
	if err != nil {
		return nil, err
	}
	return s.parseSource(ctx, file, src)
}

func (s *sourceTree) parseSource(ctx context.Context, file string, src []byte) (*ast.File, error) {
	active, err := s.buildPlatforms(ctx, file, src)
	if err != nil {
		return nil, err
	}
	tree := &ast.File{}
	if active != 0 {
		tree, err = parser.ParseFile(s.set, file, src, parser.SkipObjectResolution)
		if tree != nil && importsC(tree) {
			active = 0
		} else if err != nil {
			var errs scanner.ErrorList
			if errors.As(err, &errs) && len(errs) > 0 {
				line := 1 + bytes.Count(src[:errs[0].Pos.Offset], []byte{'\n'})
				return nil, fmt.Errorf("%q:%d: Go syntax error", file, line)
			}
			return nil, fmt.Errorf("%q: Go parse error", file)
		}
	}
	s.files[file], s.active[file] = tree, active
	return tree, nil
}

func importsC(f *ast.File) bool {
	for _, imp := range f.Imports {
		if name, err := strconv.Unquote(imp.Path.Value); err == nil && name == "C" {
			return true
		}
	}
	return false
}

func (s *sourceTree) parseSiblings(ctx context.Context, files []string, bit platformMask) error {
	var pending []string
	for _, file := range files {
		if mask, known := s.active[file]; s.files[file] == nil && (!known || mask&bit != 0) {
			pending = append(pending, file)
		}
	}
	return s.readBatch(pending, func(file string, src []byte) error {
		mask, err := s.buildPlatforms(ctx, file, src)
		if err != nil || mask&bit == 0 {
			return err
		}
		_, err = s.parseSource(ctx, file, src)
		return err
	})
}

func (s *sourceTree) readBatch(files []string, visit func(string, []byte) error) error {
	for batch := range slices.Chunk(files, 128) {
		var ids strings.Builder
		for _, file := range batch {
			id, err := s.blobID(file)
			if err != nil {
				return err
			}
			ids.WriteString(id)
			ids.WriteByte('\n')
		}
		data, err := s.read(ids.String(), "cat-file", "--batch=%(objectsize)")
		if err != nil {
			return err
		}
		for _, file := range batch {
			header, rest, ok := bytes.Cut(data, []byte{'\n'})
			size, err := strconv.Atoi(string(header))
			if !ok || err != nil || size < 0 || size >= len(rest) || rest[size] != '\n' {
				return errors.New("invalid Git blob batch")
			}
			if err := visit(file, rest[:size]); err != nil {
				return err
			}
			data = rest[size+1:]
		}
	}
	return nil
}

func syntax(file *ast.File, decls []ast.Decl) (string, error) {
	var out bytes.Buffer
	if err := printer.Fprint(&out, token.NewFileSet(), &ast.File{Name: file.Name, Decls: decls}); err != nil {
		return "", errors.New("cannot compare Go declarations")
	}
	return out.String(), nil
}

type fileChange struct {
	active     platformMask
	signatures string
	callers    string
	importable bool
}

func (s *sourceTree) describe(ctx context.Context, file, id string) (fileChange, error) {
	if id == "" {
		return fileChange{}, nil
	}
	f, err := s.parse(ctx, file)
	if err != nil {
		return fileChange{active: allPlatforms, importable: true}, err
	}
	if s.active[file] == 0 {
		return fileChange{}, nil
	}
	signatures := slices.Clone(f.Decls)
	for i, decl := range signatures {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			signature := *fn
			signature.Body = nil
			signatures[i] = &signature
		}
	}
	callers := slices.Clone(signatures)
	for _, call := range s.possibleCalls(file, f) {
		callers[call.decl] = f.Decls[call.decl]
	}
	sig, err := syntax(f, signatures)
	if err != nil {
		return fileChange{}, err
	}
	bodies, err := syntax(f, callers)
	if err != nil {
		return fileChange{}, err
	}
	return fileChange{s.active[file], f.GoVersion + sig, bodies, f.Name.Name != "main"}, nil
}

func (s *sourceTree) changes(ctx context.Context, target *sourceTree) (map[string]platformMask, error) {
	changed := map[string]platformMask{}
	declared := map[string]platformMask{}
	importable := false
	paths := maps.Clone(s.blobs)
	maps.Copy(paths, target.blobs)
	for _, file := range slices.Sorted(maps.Keys(paths)) {
		afterID, err := s.blobID(file)
		if err != nil && productionPath(file) {
			return nil, fmt.Errorf("candidate: %w", err)
		}
		beforeID, err := target.blobID(file)
		if err != nil && productionPath(file) && afterID != "" {
			return nil, fmt.Errorf("target: %w", err)
		}
		if beforeID == afterID {
			continue
		}
		after, err := s.describe(ctx, file, afterID)
		if err != nil && productionPath(file) {
			return nil, fmt.Errorf("candidate: %w", err)
		}
		before, err := target.describe(ctx, file, beforeID)
		if err != nil {
			before = fileChange{active: allPlatforms, importable: true}
		}
		mask := before.active ^ after.active
		if before.signatures != after.signatures {
			mask = before.active | after.active
		}
		if mask != 0 {
			declared[path.Dir(file)] |= mask
			importable = importable || before.importable || after.importable
		}
		if before.callers != after.callers || before.active != after.active {
			changed[file] = after.active
		}
	}
	if !maps.Equal(s.moduleFiles, target.moduleFiles) {
		for file := range s.blobs {
			changed[file] = allPlatforms
		}
		return changed, nil
	}
	for file := range s.blobs {
		if mask := declared[path.Dir(file)]; mask != 0 {
			changed[file] |= mask
		}
	}
	if importable {
		if err := s.includeImporters(ctx, changed, declared); err != nil {
			return nil, err
		}
	}
	return changed, nil
}

func (s *sourceTree) includeImporters(ctx context.Context, changed, declared map[string]platformMask) error {
	affected := maps.Clone(declared)
	reverse := map[string]map[string]platformMask{}
	files := slices.Sorted(maps.Keys(s.blobs))
	files = slices.DeleteFunc(files, func(file string) bool { return s.blobs[file].symlink })
	err := s.readBatch(files, func(file string, src []byte) error {
		f, err := parser.ParseFile(token.NewFileSet(), file, src, parser.ImportsOnly|parser.SkipObjectResolution)
		if err != nil {
			return nil
		}
		if len(f.Imports) == 0 || importsC(f) {
			return nil
		}
		mask, err := s.buildPlatforms(ctx, file, src)
		if err != nil {
			mask = allPlatforms
		}
		dir := path.Dir(file)
		for _, imp := range f.Imports {
			pkg, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			dep, local := strings.CutPrefix(pkg, module)
			if pkg == strings.TrimSuffix(module, "/") {
				dep, local = ".", true
			}
			if local {
				if direct := mask & declared[dep]; direct != 0 {
					changed[file] |= direct
				}
				if reverse[dep] == nil {
					reverse[dep] = map[string]platformMask{}
				}
				reverse[dep][dir] |= mask
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	queue := slices.Collect(maps.Keys(affected))
	for len(queue) > 0 {
		dep := queue[0]
		queue = queue[1:]
		for dir, mask := range reverse[dep] {
			if add := mask & affected[dep] &^ affected[dir]; add != 0 {
				affected[dir] |= add
				queue = append(queue, dir)
			}
		}
	}
	for dir, mask := range affected {
		if s.methodMayBeVisible(dir) {
			for _, file := range s.packages[dir] {
				changed[file] |= mask
			}
		}
	}

	return nil
}

func (s *sourceTree) methodMayBeVisible(dir string) bool {
	for pkg, ops := range s.audited {
		for _, op := range ops {
			_, name, method := strings.Cut(op, ".")
			if method && (pkg == module+dir || token.IsExported(name)) {
				return true
			}
		}
	}
	return false
}

type packageSources struct {
	owner   string
	files   []*ast.File
	sources []string
}

type importGraph struct {
	seen     map[string]bool
	external map[string]bool
	packages []packageSources
}

func (s *sourceTree) collect(ctx context.Context, owner string, bit platformMask, graph *importGraph) error {
	if graph.seen[owner] {
		return nil
	}
	graph.seen[owner] = true
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := strings.TrimPrefix(owner, module)
	if owner == strings.TrimSuffix(module, "/") {
		dir = "."
	}
	for parent := dir; parent != "."; parent = path.Dir(parent) {
		if _, nested := s.moduleFiles[path.Join(parent, "go.mod")]; nested {
			return errors.New("nested Go module")
		}
	}
	if err := s.parseSiblings(ctx, s.packages[dir], bit); err != nil {
		return err
	}
	group := packageSources{owner: owner}
	for _, file := range s.packages[dir] {
		if s.active[file]&bit == 0 {
			continue
		}
		f := s.files[file]
		group.files = append(group.files, f)
		group.sources = append(group.sources, file, s.blobs[file].id)
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil || modpath.CheckImportPath(p) != nil || strings.Contains(p, "...") {
				return errors.New("invalid Go import")
			}
			if p == strings.TrimSuffix(module, "/") || strings.HasPrefix(p, module) {
				if err := s.collect(ctx, p, bit, graph); err != nil {
					return err
				}
			} else {
				switch p {
				case "all", "std", "cmd":
					return errors.New("reserved Go package pattern")
				default:
					graph.external[p] = true
				}
			}
		}
	}
	if len(group.files) == 0 {
		return errors.New("missing Go source")
	}
	graph.packages = append(graph.packages, group)
	return nil
}

type cachedPackage struct {
	sources []string
	pkg     *types.Package
}

func (p cachedPackage) matches(group packageSources, resolved packageImports) bool {
	if p.pkg == nil || !slices.Equal(group.sources, p.sources) {
		return false
	}
	for _, dep := range p.pkg.Imports() {
		if resolved[dep.Path()] != dep {
			return false
		}
	}
	return true
}

type typeCache struct {
	external packageImports
	local    map[string]cachedPackage
}

func newTypeCache() *typeCache {
	return &typeCache{packageImports{}, map[string]cachedPackage{}}
}

func (s *sourceTree) checkConfig(ctx context.Context, platform buildTarget, imports map[string]bool, resolved, cached packageImports) (cfg types.Config, err error) {
	data, err := s.read.run("cat-file", "blob", s.moduleFiles["go.mod"])
	if err != nil {
		return cfg, err
	}
	mod, err := modfile.Parse("go.mod", data, nil)
	if err != nil || mod.Module == nil || mod.Go == nil || mod.Module.Mod.Path+"/" != module {
		return cfg, errors.New("invalid Go module")
	}
	cfg = types.Config{Importer: resolved, GoVersion: "go" + mod.Go.Version, Sizes: types.SizesFor("gc", platform.arch)}
	delete(imports, "unsafe")
	missing := false
	for name := range imports {
		if cached[name] == nil {
			missing = true
			break
		}
	}
	if !missing {
		maps.Copy(resolved, cached)
		return cfg, nil
	}
	for _, name := range []string{"vendor/modules.txt", "go.work"} {
		if _, present := s.moduleFiles[name]; present {
			return cfg, errors.New("dependencies require a different module context")
		}
	}
	for _, replace := range mod.Replace {
		if replace.New.Version == "" {
			return cfg, errors.New("local module replacement")
		}
	}
	dir, err := os.MkdirTemp("", "authority-mod-")
	if err != nil {
		return cfg, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), data, 0600); err != nil {
		return cfg, err
	}
	if sum, ok := s.moduleFiles["go.sum"]; ok {
		data, err := s.read.run("cat-file", "blob", sum)
		if err != nil {
			return cfg, err
		}
		if err := os.WriteFile(filepath.Join(dir, "go.sum"), data, 0600); err != nil {
			return cfg, err
		}
	}
	deps := &packages.Config{
		Context: ctx, Dir: dir, Mode: packages.NeedName | packages.NeedTypes,
		BuildFlags: []string{"-p=1", "-mod=readonly"},
		Env:        append(platform.env(), "GO111MODULE=on"),
	}
	pkgs, err := packages.Load(deps, slices.Sorted(maps.Keys(imports))...)
	if err != nil {
		return cfg, err
	}
	for _, p := range pkgs {
		if p.IllTyped || len(p.Errors) != 0 || p.Types == nil {
			return cfg, errors.New("incomplete dependency types")
		}
		resolved[p.PkgPath] = p.Types
		cached[p.PkgPath] = p.Types
	}
	return cfg, nil
}

func uninstantiated(expr ast.Expr) ast.Expr {
	switch e := ast.Unparen(expr).(type) {
	case *ast.IndexExpr:
		return ast.Unparen(e.X)
	case *ast.IndexListExpr:
		return ast.Unparen(e.X)
	default:
		return e
	}
}

func callerLabel(f *ast.FuncDecl) string {
	if f.Recv == nil {
		return f.Name.Name
	}
	if len(f.Recv.List) == 1 {
		receiver := ast.Unparen(f.Recv.List[0].Type)
		if ptr, ok := receiver.(*ast.StarExpr); ok {
			receiver = ast.Unparen(ptr.X)
		}
		if id, ok := uninstantiated(receiver).(*ast.Ident); ok {
			return id.Name + "." + f.Name.Name
		}
	}
	return "<receiver>." + f.Name.Name
}

func (s *sourceTree) possibleNames(owner string, f *ast.File) map[string]bool {
	imported := map[string]bool{owner: true}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err == nil && (imp.Name == nil || imp.Name.Name != "_") {
			imported[p] = true
		}
	}
	names := make(map[string]bool)
	for pkg, ops := range s.audited {
		for _, op := range ops {
			_, name, method := strings.Cut(op, ".")
			if method {
				if pkg == owner || token.IsExported(name) {
					names[name] = true
				}
			} else if pkg == owner || imported[pkg] && token.IsExported(op) {
				names[op] = true
			}
		}
	}
	return names
}

func operation(obj types.Object) (string, string) {
	fn, ok := obj.(*types.Func)
	if !ok || fn.Pkg() == nil {
		return "", ""
	}
	receiver := fn.Signature().Recv()
	if receiver == nil {
		return fn.Pkg().Path(), fn.Name()
	}
	typ := types.Unalias(receiver.Type())
	if pointer, ok := typ.(*types.Pointer); ok {
		typ = types.Unalias(pointer.Elem())
	}
	if named, ok := typ.(*types.Named); ok && named.Obj().Pkg() != nil && named.Obj().Parent() == named.Obj().Pkg().Scope() {
		return named.Obj().Pkg().Path(), named.Obj().Name() + "." + fn.Name()
	}
	return "", ""
}

type possibleCall struct {
	decl   int
	caller string
	id     *ast.Ident
	fun    ast.Expr
}

func (s *sourceTree) possibleCalls(file string, tree *ast.File) []possibleCall {
	owner := module + path.Dir(file)
	names := s.possibleNames(owner, tree)
	var calls []possibleCall
	if len(names) == 0 {
		return nil
	}
	initNumber := 0
	for index, decl := range tree.Decls {
		caller := "package initialization"
		if f, ok := decl.(*ast.FuncDecl); ok {
			caller = callerLabel(f)
			if caller == "init" {
				initNumber++
				caller += "#" + strconv.Itoa(initNumber)
			}
		}
		for n := range ast.Preorder(decl) {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				continue
			}
			fun := uninstantiated(call.Fun)
			var id *ast.Ident
			switch x := fun.(type) {
			case *ast.Ident:
				id = x
			case *ast.SelectorExpr:
				id = x.Sel
			}
			if id == nil || !names[id.Name] {
				continue
			}
			calls = append(calls, possibleCall{index, caller, id, fun})
		}
	}
	return calls
}

func (s *sourceTree) countSites(ctx context.Context, platform buildTarget, bit platformMask, files []string, cache *typeCache) (map[string]map[callSite]int, error) {
	pending := map[string][]possibleCall{}
	roots := map[string]string{}
	graph := importGraph{seen: map[string]bool{}, external: map[string]bool{}}
	unresolved := func(file string, call possibleCall) error {
		return fmt.Errorf("%q:%d: unresolved possible authority call", file, s.set.PositionFor(call.id.Pos(), false).Line)
	}
	if err := s.parseSiblings(ctx, files, bit); err != nil {
		return nil, err
	}
	for _, file := range files {
		if s.active[file]&bit == 0 {
			continue
		}
		calls := s.possibleCalls(file, s.files[file])
		if len(calls) == 0 {
			continue
		}
		pending[file] = calls
		owner := module + path.Dir(file)
		if err := s.collect(ctx, owner, bit, &graph); err != nil {
			return nil, unresolved(file, calls[0])
		}
		if roots[owner] == "" {
			roots[owner] = file
		}
	}
	if len(roots) == 0 {
		return map[string]map[callSite]int{}, nil
	}
	resolved := packageImports{"unsafe": types.Unsafe}
	cfg, err := s.checkConfig(ctx, platform, graph.external, resolved, cache.external)
	if err != nil {
		file := slices.Sorted(maps.Keys(pending))[0]
		return nil, unresolved(file, pending[file][0])
	}
	checked := map[string]*types.Info{}
	for _, group := range graph.packages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file := roots[group.owner]
		if cached := cache.local[group.owner]; file == "" && cached.matches(group, resolved) {
			resolved[group.owner] = cached.pkg
			continue
		}
		cfg.IgnoreFuncBodies = file == ""
		var info *types.Info
		if file != "" {
			info = &types.Info{Uses: make(map[*ast.Ident]types.Object), Selections: make(map[*ast.SelectorExpr]*types.Selection)}
		}
		pkg, err := cfg.Check(group.owner, s.set, group.files, info)
		if err != nil {
			if file != "" {
				return nil, unresolved(file, pending[file][0])
			}
			continue
		}
		resolved[group.owner] = pkg
		cache.local[group.owner] = cachedPackage{group.sources, pkg}
		checked[group.owner] = info
	}
	result := map[string]map[callSite]int{}
	for file, calls := range pending {
		info := checked[module+path.Dir(file)]
		counts := map[callSite]int{}
		for _, call := range calls {
			obj := info.Uses[call.id]
			if obj == nil {
				return nil, unresolved(file, call)
			}
			if sel, ok := call.fun.(*ast.SelectorExpr); ok {
				if selected := info.Selections[sel]; selected != nil {
					obj = selected.Obj()
				}
			}
			pkg, name := operation(obj)
			if slices.Contains(s.audited[pkg], name) {
				counts[callSite{caller: call.caller, pkg: pkg, operation: name}]++
			}
		}
		result[file] = counts
	}
	return result, nil
}
