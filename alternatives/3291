package issueorigin

import "testing"

func TestDefectFingerprint(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		body string
		key  string
	}{
		{"test output", "--- FAIL: TestJourney (0.01s)\nFAIL example/hub 0.02s", "go-test:example/hub:TestJourney"},
		{"scheduled test", "Problem: `go-test:example/hub:TestJourney`", "go-test:example/hub:TestJourney"},
		{"subtest output", "--- FAIL: TestJourney (0.01s)\n    --- FAIL: TestJourney/floor (0.01s)\nFAIL example/hub 0.02s", "go-test:example/hub:TestJourney/floor"},
		{"goose panic", "panic: goose: duplicate version 20261006023000 detected:", "goose:duplicate-version:20261006023000"},
		{"migration check", "duplicate migration version 20261006023000: left.sql and right.go", "goose:duplicate-version:20261006023000"},
		{"different version", "duplicate migration version 20261006023001", "goose:duplicate-version:20261006023001"},
		{"different package", "--- FAIL: TestJourney (0.01s)\nFAIL example/other 0.02s", "go-test:example/other:TestJourney"},
		{"unqualified test", "--- FAIL: TestJourney (0.01s)", ""},
		{"test mention", "TestJourney should cover this feature", ""},
		{"multiple packages", "--- FAIL: TestJourney (0.01s)\nFAIL example/hub\nFAIL example/other", ""},
		{"compiler diagnostic", "internal/hub/pool.go:23:4: undefined: capacity", "go-diagnostic:internal/hub/pool.go:undefined: capacity"},
		{"compiler diagnostic moved", "internal/hub/pool.go:31:2: undefined: capacity", "go-diagnostic:internal/hub/pool.go:undefined: capacity"},
		{"scheduled compiler diagnostic", "Problem: `go-diagnostic:internal/hub/pool.go:23:4:undefined: capacity`", "go-diagnostic:internal/hub/pool.go:undefined: capacity"},
		{"lint diagnostic", "internal/one.go:12:3: unused value (staticcheck)", "go-diagnostic:internal/one.go:unused value (staticcheck)"},
		{"gosec diagnostic", "[internal/one.go:12] - (G104 (CWE-703): Errors unhandled.)", "go-diagnostic:internal/one.go:G104 (CWE-703): Errors unhandled."},
		{"assertion without qualified test", "one_test.go:12: expected result missing", ""},
		{"absolute compiler path", "/tool/cache/hub.go:23:4: undefined: capacity", ""},
		{"instance failure", "backend startup failed: protocol handshake error", ""},
		{"fingerprint cannot supply diagnostic", Stamp("Unknown failure", Origin{Kind: "worker", Fingerprint: "duplicate migration version 20261006023000"}), ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := ""
			if test.key != "" {
				want = Fingerprint(test.key)
			}
			if got := DefectFingerprint(test.body); got != want {
				t.Fatalf("fingerprint = %q, want %q", got, want)
			}
		})
	}
}
