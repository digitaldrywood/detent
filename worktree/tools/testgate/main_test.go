package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPackageResultClassifiesFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		events []testEvent
		want   string
	}{
		{
			name: "assertion",
			events: []testEvent{
				{Action: "output", Test: "TestValue", Output: "value = 1, want 2"},
				{Action: "fail", Test: "TestValue"},
				{Action: "fail"},
			},
			want: "assertion",
		},
		{
			name: "operation timeout",
			events: []testEvent{
				{Action: "output", Test: "TestStream", Output: "claude stream stalled after 10s"},
				{Action: "fail", Test: "TestStream"},
				{Action: "fail"},
			},
			want: "operation_timeout",
		},
		{
			name: "package timeout takes precedence",
			events: []testEvent{
				{Action: "output", Test: "TestStream", Output: "context deadline exceeded"},
				{Action: "output", Output: "panic: test timed out after 10m0s"},
				{Action: "fail"},
			},
			want: "package_timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := packageResult{Outcome: "running", testTimeouts: make(map[string]bool)}
			for _, event := range tt.events {
				result.apply(event)
			}
			result.finalize()
			if result.Classification != tt.want {
				t.Fatalf("Classification = %q, want %q", result.Classification, tt.want)
			}
		})
	}
}

func TestPackageResultRecordsPackageSkip(t *testing.T) {
	t.Parallel()

	result := packageResult{Outcome: "running", testTimeouts: make(map[string]bool)}
	result.apply(testEvent{Action: "skip", Elapsed: 0.25})

	if result.Outcome != "skip" || result.Elapsed != 0.25 {
		t.Fatalf("result = %#v, want skipped package with elapsed time", result)
	}
}

func TestEvidenceCollectorRetainsBuildEventsWithFailedPackage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	collector := newEvidenceCollector(dir, io.Discard, io.Discard)
	buildPath := "example.com/project/pkg [example.com/project/pkg.test]"
	lines := []string{
		`{"ImportPath":"` + buildPath + `","Action":"build-output","Output":"pkg_test.go:12:2: undefined: missing\\n"}`,
		`{"ImportPath":"` + buildPath + `","Action":"build-fail"}`,
		`{"Action":"fail","Package":"example.com/project/pkg","FailedBuild":"` + buildPath + `"}`,
	}
	if err := collector.collect(strings.NewReader(strings.Join(lines, "\n") + "\n")); err != nil {
		t.Fatalf("collect() error = %v", err)
	}
	if err := collector.close(); err != nil {
		t.Fatalf("close() error = %v", err)
	}

	summary := collector.summary(4, 10*time.Minute)
	if len(summary.Packages) != 1 {
		t.Fatalf("packages len = %d, want 1", len(summary.Packages))
	}
	result := summary.Packages[0]
	data, err := os.ReadFile(filepath.Join(dir, result.EvidenceFile))
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", result.EvidenceFile, err)
	}
	for _, want := range []string{"undefined: missing", "build-fail", `"FailedBuild"`} {
		if !bytes.Contains(data, []byte(want)) {
			t.Fatalf("package evidence missing %q: %s", want, data)
		}
	}
}

func TestEvidenceCollectorRetainsPerPackageJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	var combined bytes.Buffer
	collector := newEvidenceCollector(dir, &combined, io.Discard)
	lines := []string{
		`{"Action":"run","Package":"github.com/digitaldrywood/detent/internal/store"}`,
		`{"Action":"pass","Package":"github.com/digitaldrywood/detent/internal/store","Elapsed":1.25}`,
		`{"Action":"run","Package":"github.com/digitaldrywood/detent/internal/web"}`,
		`{"Action":"output","Package":"github.com/digitaldrywood/detent/internal/web","Test":"TestSSE","Output":"context deadline exceeded\n"}`,
		`{"Action":"fail","Package":"github.com/digitaldrywood/detent/internal/web","Test":"TestSSE"}`,
		`{"Action":"fail","Package":"github.com/digitaldrywood/detent/internal/web","Elapsed":2.5}`,
	}
	if err := collector.collect(strings.NewReader(strings.Join(lines, "\n") + "\n")); err != nil {
		t.Fatalf("collect() error = %v", err)
	}
	if err := collector.close(); err != nil {
		t.Fatalf("close() error = %v", err)
	}

	summary := collector.summary(4, 10*time.Minute)
	if summary.PackageParallelism != 1 || summary.TestParallelism != 4 || summary.PackageTimeout != "10m0s" {
		t.Fatalf("summary budget = %#v", summary)
	}
	if len(summary.Packages) != 2 {
		t.Fatalf("packages len = %d, want 2", len(summary.Packages))
	}
	if got := summary.Packages[0]; got.Package != "github.com/digitaldrywood/detent/internal/store" || got.Outcome != "pass" || got.Classification != "" {
		t.Fatalf("store result = %#v", got)
	}
	if got := summary.Packages[1]; got.Package != "github.com/digitaldrywood/detent/internal/web" || got.Outcome != "fail" || got.Classification != "operation_timeout" {
		t.Fatalf("web result = %#v", got)
	}
	for _, result := range summary.Packages {
		data, err := os.ReadFile(filepath.Join(dir, result.EvidenceFile))
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", result.EvidenceFile, err)
		}
		if !bytes.Contains(data, []byte(result.Package)) {
			t.Fatalf("%s does not contain package %q", result.EvidenceFile, result.Package)
		}
	}
	if got := strings.Count(combined.String(), "\n"); got != len(lines) {
		t.Fatalf("combined line count = %d, want %d", got, len(lines))
	}
}

func TestRunValidatesArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "missing output", args: []string{"-output", ""}, wantErr: "-output is required"},
		{name: "invalid parallel", args: []string{"-parallel", "0"}, wantErr: "-parallel must be positive"},
		{name: "invalid timeout", args: []string{"-timeout", "0s"}, wantErr: "-timeout must be positive"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stderr bytes.Buffer
			if code := run(t.Context(), tt.args, io.Discard, &stderr); code != 2 {
				t.Fatalf("run() = %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr = %q, want containing %q", stderr.String(), tt.wantErr)
			}
		})
	}
}

func TestGateSummaryJSONUsesClassificationNames(t *testing.T) {
	t.Parallel()

	summary := gateSummary{
		Schema:             1,
		PackageParallelism: 1,
		TestParallelism:    4,
		PackageTimeout:     "10m0s",
		Packages: []packageResult{
			{Package: "example/assertion", Outcome: "fail", Classification: "assertion", EvidenceFile: "assertion.jsonl"},
			{Package: "example/operation", Outcome: "fail", Classification: "operation_timeout", EvidenceFile: "operation.jsonl"},
			{Package: "example/package", Outcome: "fail", Classification: "package_timeout", EvidenceFile: "package.jsonl"},
		},
	}
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, want := range []string{"assertion", "operation_timeout", "package_timeout"} {
		if !bytes.Contains(data, []byte(want)) {
			t.Fatalf("summary JSON missing %q: %s", want, data)
		}
	}
}
