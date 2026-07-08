package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testModule = "github.com/digitaldrywood/detent"

func TestCheckCoverage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		profile    string
		floor      float64
		exceptions map[string]float64
		want       []coverageFailure
	}{
		{
			name: "aggregates statement coverage per package",
			profile: coverProfile(
				testModule+"/internal/foo/a.go:1.1,1.2 2 1",
				testModule+"/internal/foo/b.go:2.1,2.2 3 0",
				testModule+"/internal/bar/a.go:1.1,1.2 4 1",
			),
			floor: 50,
			want: []coverageFailure{
				{
					Package:  testModule + "/internal/foo",
					Coverage: 40,
					Floor:    50,
				},
			},
		},
		{
			name: "skips templ and sqlc files",
			profile: coverProfile(
				testModule+"/internal/web/view_templ.go:1.1,1.2 100 0",
				testModule+"/internal/store/sqlc/models.go:1.1,1.2 100 0",
				testModule+"/internal/database/sqlc/models.go:1.1,1.2 100 0",
				testModule+"/internal/web/server.go:1.1,1.2 4 4",
			),
			floor: 100,
		},
		{
			name: "honors exception floors",
			profile: coverProfile(
				testModule+"/internal/foo/a.go:1.1,1.2 1 0",
				testModule+"/internal/bar/a.go:1.1,1.2 1 0",
			),
			floor: 50,
			exceptions: map[string]float64{
				testModule + "/internal/foo": 0,
			},
			want: []coverageFailure{
				{
					Package:  testModule + "/internal/bar",
					Coverage: 0,
					Floor:    50,
				},
			},
		},
		{
			name: "sorts failures by package",
			profile: coverProfile(
				testModule+"/internal/zeta/a.go:1.1,1.2 1 0",
				testModule+"/internal/alpha/a.go:1.1,1.2 1 0",
			),
			floor: 50,
			want: []coverageFailure{
				{
					Package:  testModule + "/internal/alpha",
					Coverage: 0,
					Floor:    50,
				},
				{
					Package:  testModule + "/internal/zeta",
					Coverage: 0,
					Floor:    50,
				},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := checkCoverage(strings.NewReader(tt.profile), tt.floor, tt.exceptions)
			if err != nil {
				t.Fatalf("checkCoverage() error = %v", err)
			}

			assertFailures(t, got, tt.want)
		})
	}
}

func TestParseExceptions(t *testing.T) {
	t.Parallel()

	input := strings.NewReader(`
# Packages below the default floor.

github.com/digitaldrywood/detent/internal/foo 40 # Raised when foo tests expand.
github.com/digitaldrywood/detent/internal/bar 0
`)

	got, err := parseExceptions(input)
	if err != nil {
		t.Fatalf("parseExceptions() error = %v", err)
	}

	want := map[string]float64{
		testModule + "/internal/foo": 40,
		testModule + "/internal/bar": 0,
	}
	if len(got) != len(want) {
		t.Fatalf("len(parseExceptions()) = %d, want %d", len(got), len(want))
	}
	for packagePath, wantFloor := range want {
		if gotFloor, ok := got[packagePath]; !ok || gotFloor != wantFloor {
			t.Fatalf("parseExceptions()[%q] = %v, %v; want %v, true", packagePath, gotFloor, ok, wantFloor)
		}
	}
}

func TestParseExceptionsErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{
			name:    "bad floor",
			input:   testModule + "/internal/foo nope\n",
			wantErr: "exceptions line 1: invalid floor",
		},
		{
			name:    "missing floor",
			input:   testModule + "/internal/foo\n",
			wantErr: "exceptions line 1: want package path and floor",
		},
		{
			name:    "floor out of range",
			input:   testModule + "/internal/foo 101\n",
			wantErr: "exceptions line 1: invalid floor",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseExceptions(strings.NewReader(tt.input))
			if err == nil {
				t.Fatal("parseExceptions() error = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("parseExceptions() error = %q, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		profile        string
		exceptions     string
		wantCode       int
		wantStdout     string
		wantStderr     string
		wantStderrPart string
	}{
		{
			name: "passes when exception floor is met",
			profile: coverProfile(
				testModule + "/internal/foo/a.go:1.1,1.2 1 0",
			),
			exceptions: testModule + "/internal/foo 0\n",
			wantCode:   0,
			wantStdout: "package coverage meets per-package floors\n",
		},
		{
			name: "fails with sorted package output",
			profile: coverProfile(
				testModule+"/internal/zeta/a.go:1.1,1.2 1 0",
				testModule+"/internal/alpha/a.go:1.1,1.2 1 0",
			),
			wantCode: 1,
			wantStderr: "package coverage below floor:\n" +
				"  github.com/digitaldrywood/detent/internal/alpha: 0.0% below 50.0%\n" +
				"  github.com/digitaldrywood/detent/internal/zeta: 0.0% below 50.0%\n",
		},
		{
			name:           "reports bad exception floor",
			profile:        coverProfile(testModule + "/internal/foo/a.go:1.1,1.2 1 1"),
			exceptions:     testModule + "/internal/foo no\n",
			wantCode:       1,
			wantStderrPart: "read exceptions: exceptions line 1: invalid floor",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tempDir := t.TempDir()
			profilePath := filepath.Join(tempDir, "coverage.out")
			if err := os.WriteFile(profilePath, []byte(tt.profile), 0o600); err != nil {
				t.Fatalf("write profile: %v", err)
			}

			args := []string{"-profile", profilePath, "-floor", "50"}
			if tt.exceptions != "" {
				exceptionsPath := filepath.Join(tempDir, "exceptions.txt")
				if err := os.WriteFile(exceptionsPath, []byte(tt.exceptions), 0o600); err != nil {
					t.Fatalf("write exceptions: %v", err)
				}
				args = append(args, "-exceptions", exceptionsPath)
			}

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			gotCode := run(args, &stdout, &stderr)

			if gotCode != tt.wantCode {
				t.Fatalf("run() = %d, want %d\nstdout:\n%s\nstderr:\n%s", gotCode, tt.wantCode, stdout.String(), stderr.String())
			}
			if gotStdout := stdout.String(); gotStdout != tt.wantStdout {
				t.Fatalf("stdout = %q, want %q", gotStdout, tt.wantStdout)
			}
			if tt.wantStderr != "" {
				if gotStderr := stderr.String(); gotStderr != tt.wantStderr {
					t.Fatalf("stderr = %q, want %q", gotStderr, tt.wantStderr)
				}
			}
			if tt.wantStderrPart != "" && !strings.Contains(stderr.String(), tt.wantStderrPart) {
				t.Fatalf("stderr = %q, want containing %q", stderr.String(), tt.wantStderrPart)
			}
		})
	}
}

func coverProfile(lines ...string) string {
	return "mode: set\n" + strings.Join(lines, "\n") + "\n"
}

func assertFailures(t *testing.T, got, want []coverageFailure) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("len(failures) = %d, want %d\ngot: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("failures[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}
