package invariants

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

const (
	packageSizePolicyFile    = "internal/invariants/package_size_policy.json"
	packageSizeLineThreshold = 15000
	packageSizeFileSlack     = 5
	packageSizeLineSlack     = 1000
	packageSizeSplitAdvice   = "split the package by feature instead of growing it"
	packageSizeShrinkAdvice  = "after shrinking, run go test ./internal/invariants -run '^TestRepositoryPackageSize$' -package-size-shrink"
)

var packageSizeShrink = flag.Bool("package-size-shrink", false, "shrink package size policy against the committed policy file")

var generatedGoHeader = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.\r?$`)

type packageSize struct {
	SourceFiles    int `json:"source_files"`
	SourceLines    int `json:"source_lines"`
	TestFiles      int `json:"test_files"`
	TestLines      int `json:"test_lines"`
	GeneratedFiles int `json:"-"`
	GeneratedLines int `json:"-"`
}

type packageSizePolicy struct {
	Packages map[string]packageSize `json:"packages"`
}

type packageSizeMetric struct {
	name  string
	slack int
	value func(packageSize) int
}

var packageSizeMetrics = []packageSizeMetric{
	{name: "source_files", slack: packageSizeFileSlack, value: func(size packageSize) int { return size.SourceFiles }},
	{name: "source_lines", slack: packageSizeLineSlack, value: func(size packageSize) int { return size.SourceLines }},
	{name: "test_files", slack: packageSizeFileSlack, value: func(size packageSize) int { return size.TestFiles }},
	{name: "test_lines", slack: packageSizeLineSlack, value: func(size packageSize) int { return size.TestLines }},
}

func (size packageSize) handWrittenLines() int {
	return size.SourceLines + size.TestLines
}

func (size packageSize) large() bool {
	return size.handWrittenLines() > packageSizeLineThreshold
}

func TestRepositoryPackageSize(t *testing.T) {
	if *packageSizeShrink {
		if err := shrinkPackageSizePolicy(repositoryRoot(t)); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Parallel()
	root := repositoryRoot(t)
	data, err := os.ReadFile(filepath.Join(root, packageSizePolicyFile))
	if err != nil {
		t.Fatal(err)
	}
	var policy packageSizePolicy
	if err := json.Unmarshal(data, &policy); err != nil {
		t.Fatal(err)
	}
	current, err := scanPackageSizes(os.DirFS(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range slices.Sorted(maps.Keys(current)) {
		size := current[name]
		if _, listed := policy.Packages[name]; listed || size.large() {
			t.Logf("%s: source %d files %d lines, test %d files %d lines, generated %d files %d lines", name, size.SourceFiles, size.SourceLines, size.TestFiles, size.TestLines, size.GeneratedFiles, size.GeneratedLines)
		}
	}
	for _, problem := range packageSizeProblems(current, policy.Packages) {
		t.Error(problem)
	}
}

func scanPackageSizes(root fs.FS) (map[string]packageSize, error) {
	sizes := make(map[string]packageSize)
	err := fs.WalkDir(root, ".", func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if file == "." {
				return nil
			}
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || slices.Contains([]string{"node_modules", "testdata", "tmp", "vendor"}, name) {
				return fs.SkipDir
			}
			if _, err := fs.Stat(root, path.Join(file, "go.mod")); err == nil {
				return fs.SkipDir
			}
			return nil
		}
		if path.Ext(file) != ".go" || !entry.Type().IsRegular() {
			return nil
		}
		data, err := fs.ReadFile(root, file)
		if err != nil {
			return fmt.Errorf("read %s: %w", file, err)
		}
		lines := bytes.Count(data, []byte("\n"))
		if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
			lines++
		}
		size := sizes[path.Dir(file)]
		switch {
		case generatedGoFile(file, data):
			size.GeneratedFiles++
			size.GeneratedLines += lines
		case strings.HasSuffix(file, "_test.go"):
			size.TestFiles++
			size.TestLines += lines
		default:
			size.SourceFiles++
			size.SourceLines += lines
		}
		sizes[path.Dir(file)] = size
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan package sizes: %w", err)
	}
	return sizes, nil
}

func generatedGoFile(file string, data []byte) bool {
	if strings.HasSuffix(file, "_templ.go") {
		return true
	}
	header := data
	if bytes.HasPrefix(data, []byte("package ")) {
		header = nil
	} else if index := bytes.Index(data, []byte("\npackage ")); index >= 0 {
		header = data[:index+1]
	}
	return generatedGoHeader.Match(header)
}

func packageSizeGrowthProblems(current, baseline map[string]packageSize) []string {
	var problems []string
	for name, size := range current {
		if !size.large() {
			continue
		}
		base, listed := baseline[name]
		if !listed {
			problems = append(problems, fmt.Sprintf("%s: hand-written lines %d cross the package size threshold %d and the package has no baseline entry; %s", name, size.handWrittenLines(), packageSizeLineThreshold, packageSizeSplitAdvice))
			continue
		}
		for _, metric := range packageSizeMetrics {
			if metric.value(size) > metric.value(base) {
				problems = append(problems, fmt.Sprintf("%s: package size %s %d exceeds frozen baseline %d; %s", name, metric.name, metric.value(size), metric.value(base), packageSizeSplitAdvice))
			}
		}
	}
	slices.Sort(problems)
	return problems
}

func packageSizeProblems(current, baseline map[string]packageSize) []string {
	problems := packageSizeGrowthProblems(current, baseline)
	for name, base := range baseline {
		size, exists := current[name]
		if !exists {
			problems = append(problems, fmt.Sprintf("%s: remove package size baseline entry, the package no longer exists; %s", name, packageSizeShrinkAdvice))
			continue
		}
		if !size.large() {
			problems = append(problems, fmt.Sprintf("%s: remove package size baseline entry, hand-written lines %d are within the threshold %d; %s", name, size.handWrittenLines(), packageSizeLineThreshold, packageSizeShrinkAdvice))
			continue
		}
		for _, metric := range packageSizeMetrics {
			if metric.value(base)-metric.value(size) > metric.slack {
				problems = append(problems, fmt.Sprintf("%s: lower package size %s baseline from %d to %d; %s", name, metric.name, metric.value(base), metric.value(size), packageSizeShrinkAdvice))
			}
		}
	}
	slices.Sort(problems)
	return problems
}

func shrinkPackageSizePolicy(root string) error {
	committed, err := committedPackageSizePolicy(root)
	if err != nil {
		return err
	}
	data, err := packageSizeShrinkData(os.DirFS(root), committed)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, packageSizePolicyFile), data, 0o644); err != nil {
		return fmt.Errorf("write package size baseline: %w", err)
	}
	return nil
}

func committedPackageSizePolicy(root string) ([]byte, error) {
	git := func(args ...string) ([]byte, error) {
		command := exec.Command("git", args...)
		command.Dir = root
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "GIT_") {
				command.Env = append(command.Env, entry)
			}
		}
		return command.Output()
	}
	listed, err := git("ls-tree", "--name-only", "HEAD", "--", packageSizePolicyFile)
	if err != nil {
		return nil, fmt.Errorf("find committed package size policy file: %w", err)
	}
	if len(bytes.TrimSpace(listed)) > 0 {
		committed, err := git("show", "HEAD:"+packageSizePolicyFile)
		if err != nil {
			return nil, fmt.Errorf("read committed package size policy file: %w", err)
		}
		return committed, nil
	}
	uncommitted, err := os.ReadFile(filepath.Join(root, packageSizePolicyFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read uncommitted package size policy file: %w", err)
	}
	return uncommitted, nil
}

func packageSizeShrinkData(root fs.FS, committed []byte) ([]byte, error) {
	sizes, err := scanPackageSizes(root)
	if err != nil {
		return nil, err
	}
	current := packageSizePolicy{Packages: make(map[string]packageSize)}
	for name, size := range sizes {
		if size.large() {
			current.Packages[name] = size
		}
	}
	if committed != nil {
		var baseline packageSizePolicy
		if err := json.Unmarshal(committed, &baseline); err != nil {
			return nil, fmt.Errorf("decode committed package size baseline: %w", err)
		}
		if problems := packageSizeGrowthProblems(current.Packages, baseline.Packages); len(problems) > 0 {
			return nil, fmt.Errorf("refuse package size baseline growth:\n%s", strings.Join(problems, "\n"))
		}
	}
	data, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode package size baseline: %w", err)
	}
	return append(data, '\n'), nil
}

func TestPackageSizeProblems(t *testing.T) {
	t.Parallel()
	large := packageSize{SourceFiles: 40, SourceLines: 12000, TestFiles: 30, TestLines: 9000}
	small := packageSize{SourceFiles: 4, SourceLines: 900, TestFiles: 3, TestLines: 700}
	with := func(size packageSize, change func(*packageSize)) packageSize {
		change(&size)
		return size
	}
	for _, tt := range []struct {
		name     string
		current  map[string]packageSize
		baseline map[string]packageSize
		want     []string
	}{
		{name: "matching baseline", current: map[string]packageSize{"internal/big": large, "internal/small": small}, baseline: map[string]packageSize{"internal/big": large}},
		{name: "small package grows freely", current: map[string]packageSize{"internal/small": with(small, func(size *packageSize) { size.SourceLines = 9000 })}},
		{
			name:     "generated code is not counted",
			current:  map[string]packageSize{"internal/big": with(large, func(size *packageSize) { size.GeneratedFiles, size.GeneratedLines = 90, 90000 }), "internal/small": with(small, func(size *packageSize) { size.GeneratedLines = 90000 })},
			baseline: map[string]packageSize{"internal/big": large},
		},
		{
			name:     "source file growth",
			current:  map[string]packageSize{"internal/big": with(large, func(size *packageSize) { size.SourceFiles = 41 })},
			baseline: map[string]packageSize{"internal/big": large},
			want:     []string{"internal/big: package size source_files 41 exceeds frozen baseline 40; " + packageSizeSplitAdvice},
		},
		{
			name:     "test file growth",
			current:  map[string]packageSize{"internal/big": with(large, func(size *packageSize) { size.TestFiles = 31 })},
			baseline: map[string]packageSize{"internal/big": large},
			want:     []string{"internal/big: package size test_files 31 exceeds frozen baseline 30; " + packageSizeSplitAdvice},
		},
		{
			name:     "line growth in both kinds",
			current:  map[string]packageSize{"internal/big": with(large, func(size *packageSize) { size.SourceLines, size.TestLines = 12001, 9001 })},
			baseline: map[string]packageSize{"internal/big": large},
			want: []string{
				"internal/big: package size source_lines 12001 exceeds frozen baseline 12000; " + packageSizeSplitAdvice,
				"internal/big: package size test_lines 9001 exceeds frozen baseline 9000; " + packageSizeSplitAdvice,
			},
		},
		{
			name:    "unlisted package crosses threshold",
			current: map[string]packageSize{"internal/new": with(small, func(size *packageSize) { size.SourceLines, size.TestLines = 8000, 7001 })},
			want:    []string{"internal/new: hand-written lines 15001 cross the package size threshold 15000 and the package has no baseline entry; " + packageSizeSplitAdvice},
		},
		{
			name:    "unlisted package at threshold",
			current: map[string]packageSize{"internal/new": with(small, func(size *packageSize) { size.SourceLines, size.TestLines = 8000, 7000 })},
		},
		{
			name: "shrink within slack",
			current: map[string]packageSize{"internal/big": with(large, func(size *packageSize) {
				size.SourceFiles, size.SourceLines, size.TestFiles, size.TestLines = 35, 11000, 25, 8000
			})},
			baseline: map[string]packageSize{"internal/big": large},
		},
		{
			name:     "shrink beyond slack requires lower baseline",
			current:  map[string]packageSize{"internal/big": with(large, func(size *packageSize) { size.SourceFiles, size.TestLines = 34, 7999 })},
			baseline: map[string]packageSize{"internal/big": large},
			want: []string{
				"internal/big: lower package size source_files baseline from 40 to 34; " + packageSizeShrinkAdvice,
				"internal/big: lower package size test_lines baseline from 9000 to 7999; " + packageSizeShrinkAdvice,
			},
		},
		{
			name:     "listed package under threshold",
			current:  map[string]packageSize{"internal/big": small},
			baseline: map[string]packageSize{"internal/big": large},
			want:     []string{"internal/big: remove package size baseline entry, hand-written lines 1600 are within the threshold 15000; " + packageSizeShrinkAdvice},
		},
		{
			name:     "listed package removed",
			current:  map[string]packageSize{"internal/small": small},
			baseline: map[string]packageSize{"internal/big": large},
			want:     []string{"internal/big: remove package size baseline entry, the package no longer exists; " + packageSizeShrinkAdvice},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := packageSizeProblems(tt.current, tt.baseline); !slices.Equal(got, tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPackageSizeShrinkData(t *testing.T) {
	t.Parallel()
	lines := func(count int) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte("package fixture\n" + strings.Repeat("\n", count-1))}
	}
	generated := &fstest.MapFile{Data: []byte("// Code generated by sqlc. DO NOT EDIT.\n\npackage fixture\n" + strings.Repeat("\n", 20000))}
	big := fstest.MapFS{
		"go.mod":                               {Data: []byte("module example.org/project\n")},
		"internal/big/a.go":                    lines(9000),
		"internal/big/b.go":                    lines(1000),
		"internal/big/a_test.go":               lines(6000),
		"internal/big/query.sql.go":            generated,
		"internal/big/view_templ.go":           lines(20000),
		"internal/big/testdata/src/fixture.go": lines(20000),
		"internal/big/nested/go.mod":           {Data: []byte("module example.org/nested\n")},
		"internal/big/nested/a.go":             lines(20000),
		"internal/small/a.go":                  lines(100),
		"internal/small/query.sql.go":          generated,
		"node_modules/dependency/a.go":         lines(20000),
	}
	bigJSON := "{\n  \"packages\": {\n    \"internal/big\": {\n      \"source_files\": 2,\n      \"source_lines\": 10000,\n      \"test_files\": 1,\n      \"test_lines\": 6000\n    }\n  }\n}\n"
	empty := "{\n  \"packages\": {}\n}\n"
	policy := func(sizes map[string]packageSize) []byte {
		data, err := json.Marshal(packageSizePolicy{Packages: sizes})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for _, tt := range []struct {
		name      string
		files     fstest.MapFS
		committed []byte
		want      string
		wantJSON  string
	}{
		{name: "first baseline records large packages", files: big, wantJSON: bigJSON},
		{name: "unchanged baseline", files: big, committed: []byte(bigJSON), wantJSON: bigJSON},
		{
			name: "lower every metric", files: big, wantJSON: bigJSON,
			committed: policy(map[string]packageSize{"internal/big": {SourceFiles: 9, SourceLines: 30000, TestFiles: 8, TestLines: 20000}}),
		},
		{
			name: "remove missing and small packages", files: fstest.MapFS{"internal/small/a.go": lines(100)}, wantJSON: empty,
			committed: policy(map[string]packageSize{"internal/big": {SourceFiles: 2, SourceLines: 10000, TestFiles: 1, TestLines: 6000}, "internal/small": {SourceFiles: 9, SourceLines: 30000}}),
		},
		{
			name: "count cannot increase", files: big, want: "internal/big: package size source_files 2 exceeds frozen baseline 1",
			committed: policy(map[string]packageSize{"internal/big": {SourceFiles: 1, SourceLines: 10000, TestFiles: 1, TestLines: 6000}}),
		},
		{
			name: "lines cannot move between kinds", files: big, want: "internal/big: package size test_lines 6000 exceeds frozen baseline 5000",
			committed: policy(map[string]packageSize{"internal/big": {SourceFiles: 2, SourceLines: 11000, TestFiles: 1, TestLines: 5000}}),
		},
		{
			name: "new large package cannot borrow removed debt", files: big, want: "internal/big: hand-written lines 16000 cross the package size threshold 15000 and the package has no baseline entry",
			committed: policy(map[string]packageSize{"internal/old": {SourceFiles: 9, SourceLines: 30000}}),
		},
		{name: "empty committed baseline refuses large package", files: big, committed: []byte(empty), want: "internal/big: hand-written lines 16000 cross"},
		{name: "invalid committed baseline", files: big, committed: []byte("{"), want: "decode committed package size baseline"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := packageSizeShrinkData(tt.files, tt.committed)
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) || data != nil {
					t.Fatalf("got %q, %v; want refusal containing %q and no replacement data", data, err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tt.wantJSON {
				t.Fatalf("got %s, want %s", data, tt.wantJSON)
			}
		})
	}
}
