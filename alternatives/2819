package securityaudit

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChunkCoverage(t *testing.T) {
	t.Parallel()
	const limit = 16 * 1024
	const header = "diff --git a/file.go b/file.go\n--- a/file.go\n+++ b/file.go\n@@ -1 +1 @@\n"
	for _, test := range []struct {
		name, diff string
		complete   bool
		wantError  bool
	}{
		{name: "single oversized line and Unicode", diff: header + "+" + strings.Repeat("界🙂\"\\\t", limit)},
		{name: "escaped prompt exceeds raw source budget", diff: header + "+" + strings.Repeat("\x00\t\"", 2000)},
		{name: "single oversized hunk", diff: header + strings.Repeat("+changed\n", limit)},
		{name: "oversized file with many hunks", diff: header + strings.Repeat("@@ -2 +2 @@\n-old\n+new\n", limit)},
		{name: "files including binary notice add delete rename", diff: strings.Repeat("diff --git a/old b/new\nsimilarity index 100%\nrename from old\nrename to new\ndiff --git a/added b/added\nnew file mode 100644\n--- /dev/null\n+++ b/added\n@@ -0,0 +1 @@\n+added\ndiff --git a/deleted b/deleted\ndeleted file mode 100644\n--- a/deleted\n+++ /dev/null\n@@ -1 +0,0 @@\n-deleted\ndiff --git a/image b/image\nBinary files a/image and b/image differ\n", 200)},
		{name: "empty valid delta", complete: true},
		{name: "missing empty source", wantError: true},
		{name: "invalid UTF-8", diff: header + "\xff", wantError: true},
		{name: "small unchanged", diff: header + "+safe\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := Snapshot{ProjectID: "project", IssueID: "issue", Repository: "owner/repository", BaseSHA: "base", HeadSHA: "head", Diff: test.diff, DiffBytes: len(test.diff), DiffComplete: test.complete}
			plan, err := BuildPlan(snapshot, limit)
			if (err != nil) != test.wantError {
				t.Fatalf("plan error = %v", err)
			}
			if test.wantError {
				return
			}
			again, err := BuildPlan(snapshot, limit)
			if err != nil || !reflect.DeepEqual(plan, again) {
				t.Fatal("chunk construction is nondeterministic")
			}
			var reconstructed strings.Builder
			end := 0
			for index := range plan.Len() {
				identity, prompt := plan.Chunk(index)
				_, payload, ok := strings.Cut(prompt, "\n\nAudit payload:\n")
				if !ok {
					t.Fatal("missing payload")
				}
				var input struct {
					Diff string `json:"textual_diff"`
				}
				if err := json.Unmarshal([]byte(payload), &input); err != nil {
					t.Fatal(err)
				}
				if identity.Start != end || identity.End-identity.Start != len(input.Diff) || input.Diff != test.diff[identity.Start:identity.End] || !utf8.ValidString(input.Diff) || identity.DiffDigest != OutputDigest(test.diff) {
					t.Fatalf("chunk %d lost exact source coverage: %+v", index, identity)
				}
				if plan.Len() > 1 && (len(prompt) > limit || identity.Count != plan.Len()) {
					t.Fatalf("unbounded prompt: bytes=%d count=%d", len(prompt), identity.Count)
				}
				fileStart, hunkStart, lineStart := -1, -1, 0
				position := 0
				for _, line := range strings.SplitAfter(test.diff, "\n") {
					if position > identity.Start || position == len(test.diff) {
						break
					}
					lineStart = position
					if strings.HasPrefix(line, "diff --git ") {
						fileStart, hunkStart = position, -1
					} else if strings.HasPrefix(line, "@@") {
						hunkStart = position
					}
					position += len(line)
				}
				if plan.Len() > 1 && (identity.FileStart != fileStart || identity.HunkStart != hunkStart || identity.LineStart != lineStart) {
					t.Fatalf("fragment lost file/hunk/line identity: %+v want file=%d hunk=%d line=%d", identity, fileStart, hunkStart, lineStart)
				}
				reconstructed.WriteString(input.Diff)
				end = identity.End
			}
			if reconstructed.String() != test.diff || end != len(test.diff) {
				t.Fatal("bytes omitted or duplicated")
			}
		})
	}
}

func TestLargeAuditAggregation(t *testing.T) {
	t.Parallel()
	const size = 22195985
	snapshot := Snapshot{ProjectID: "project", IssueID: "issue", Repository: "owner/repository", BaseSHA: "base", HeadSHA: "head", Diff: "diff --git a/ledger b/ledger\n@@ -0,0 +1 @@\n+"}
	snapshot.Diff += strings.Repeat("x", size-len(snapshot.Diff))
	plan, err := BuildPlan(snapshot, 0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Len() < 85 {
		t.Fatalf("large diff was not bounded: %d chunks", plan.Len())
	}
	pass := `{"verdict":"pass","summary":"reviewed","findings":[]}`
	fail := `{"verdict":"fail","summary":"injection","findings":[{"id":"injection","severity":"p1","body":"Unescaped output","path":"handler.go","line":9}]}`
	outputs := make([]ChunkOutput, plan.Len())
	end := 0
	for index := range outputs {
		identity, prompt := plan.Chunk(index)
		if identity.Start != end || identity.End <= end || len(prompt) > DefaultMaxDiffBytes {
			t.Fatalf("invalid coverage for chunk %d", index)
		}
		end = identity.End
		outputs[index] = ChunkOutput{Identity: identity, Output: pass}
	}
	if end != size {
		t.Fatalf("covered %d bytes, want %d", end, size)
	}
	for _, test := range []struct {
		name                string
		edit                func([]ChunkOutput) []ChunkOutput
		wantError, wantFail bool
	}{
		{name: "complete pass"},
		{name: "late finding", edit: func(o []ChunkOutput) []ChunkOutput { o[len(o)-1].Output = fail; return o }, wantFail: true},
		{name: "early finding cannot disappear", edit: func(o []ChunkOutput) []ChunkOutput { o[0].Output = fail; return o }, wantFail: true},
		{name: "duplicate finding retains highest severity", edit: func(o []ChunkOutput) []ChunkOutput {
			o[0].Output = fail
			o[len(o)-1].Output = strings.ReplaceAll(fail, `"p1"`, `"p3"`)
			return o
		}, wantFail: true},
		{name: "missing", edit: func(o []ChunkOutput) []ChunkOutput { return o[:len(o)-1] }, wantError: true},
		{name: "duplicate", edit: func(o []ChunkOutput) []ChunkOutput { o[1] = o[0]; return o }, wantError: true},
		{name: "extra", edit: func(o []ChunkOutput) []ChunkOutput { return append(o, o[0]) }, wantError: true},
		{name: "wrong source", edit: func(o []ChunkOutput) []ChunkOutput { o[1].Identity.SnapshotDigest = "other-head"; return o }, wantError: true},
		{name: "wrong range", edit: func(o []ChunkOutput) []ChunkOutput { o[1].Identity.Start++; return o }, wantError: true},
		{name: "malformed", edit: func(o []ChunkOutput) []ChunkOutput { o[len(o)-1].Output = "invalid"; return o }, wantError: true},
		{name: "unreviewed", edit: func(o []ChunkOutput) []ChunkOutput { o[len(o)-1].Output = ""; return o }, wantError: true},
		{name: "attempted resolution", edit: func(o []ChunkOutput) []ChunkOutput {
			o[0].Output = fail
			o[len(o)-1].Output = `{"verdict":"pass","summary":"dismissed","findings":[{"id":"injection","severity":"p1","body":"Unescaped output","path":"handler.go","line":9,"status":"resolved"}]}`
			return o
		}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := append([]ChunkOutput(nil), outputs...)
			if test.edit != nil {
				candidate = test.edit(candidate)
			}
			result, receipt, err := plan.Aggregate(candidate)
			if (err != nil) != test.wantError {
				t.Fatalf("aggregate error = %v", err)
			}
			if test.wantError {
				if receipt != "" || result.Verdict != "" {
					t.Fatal("incomplete audit emitted aggregate evidence")
				}
				return
			}
			if (result.Verdict == VerdictFail) != test.wantFail || receipt == "" {
				t.Fatalf("result = %+v", result)
			}
			if test.wantFail && (len(result.Findings) != 1 || result.Findings[0].Severity != "p1") {
				t.Fatalf("lost or duplicated security finding: %+v", result.Findings)
			}
		})
	}
	t.Logf("exact synthetic source coverage: %d bytes in %d bounded prompts", size, plan.Len())
}

func TestChunkSourceAndContinuation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		edit func(*Snapshot)
	}{
		{"truncated", func(s *Snapshot) { s.DiffTruncated = true }},
		{"missing bytes", func(s *Snapshot) { s.DiffBytes = len(s.Diff) + 1 }},
		{"wrong byte count", func(s *Snapshot) { s.DiffBytes = len(s.Diff) - 1 }},
		{"pre-chunked", func(s *Snapshot) { s.Chunk = &ChunkIdentity{} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := Snapshot{Repository: "owner/repository", IssueID: "issue", BaseSHA: "base", HeadSHA: "head", Diff: strings.Repeat("+x\n", 10000)}
			test.edit(&s)
			if _, err := BuildPlan(s, 16384); err == nil {
				t.Fatal("accepted incomplete source")
			}
		})
	}
	base := Snapshot{ProjectID: "project", Repository: "owner/repository", IssueID: "issue", BaseSHA: "base", HeadSHA: "head", Diff: "+x\n"}
	original, err := BuildPlan(base, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		edit func(*Snapshot)
	}{
		{"project", func(s *Snapshot) { s.ProjectID = "other" }},
		{"repository", func(s *Snapshot) { s.Repository = "owner/other" }},
		{"base", func(s *Snapshot) { s.BaseSHA = "other" }},
		{"head", func(s *Snapshot) { s.HeadSHA = "other" }},
		{"issue", func(s *Snapshot) { s.IssueID = "other" }},
		{"source", func(s *Snapshot) { s.Diff = "+y\n" }},
	} {
		t.Run(test.name+" identity", func(t *testing.T) {
			changed := base
			test.edit(&changed)
			other, err := BuildPlan(changed, 0)
			if err != nil {
				t.Fatal(err)
			}
			id, _ := original.Chunk(0)
			otherID, _ := other.Chunk(0)
			if id.SnapshotDigest == otherID.SnapshotDigest {
				t.Fatal("aggregate identity did not bind immutable input")
			}
			if _, _, err := original.Aggregate([]ChunkOutput{{Identity: otherID, Output: `{"verdict":"pass","summary":"reviewed","findings":[]}`}}); err == nil {
				t.Fatal("accepted another source's output")
			}
		})
	}
	snapshot := Snapshot{Repository: "owner/repository", IssueID: "issue", BaseSHA: "base", HeadSHA: "head", Diff: strings.Repeat("+x\n", 10000), Previous: &PreviousAudit{BaseSHA: "base", HeadSHA: "old", Verdict: VerdictFail, Findings: []Finding{{ID: "auth", Severity: "p1", Body: "Missing check"}}}, FindingFiles: map[string]string{"auth.go": "authorization check added"}}
	plan, err := BuildPlan(snapshot, 16384)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Previous.Findings[0].ID = "caller-mutated-finding"
	outputs := make([]ChunkOutput, plan.Len())
	for index := range outputs {
		identity, prompt := plan.Chunk(index)
		if !strings.Contains(prompt, `"head_sha":"old"`) || !strings.Contains(prompt, "authorization check added") {
			t.Fatal("trusted continuation lost its original identity or source")
		}
		outputs[index] = ChunkOutput{Identity: identity, Output: `{"verdict":"pass","summary":"fixed","findings":[{"id":"auth","severity":"p1","body":"check added","status":"resolved"}]}`}
	}
	result, _, err := plan.Aggregate(outputs)
	if err != nil || result.Verdict != VerdictPass || len(result.Findings) != 1 || result.Findings[0].ID != "auth" {
		t.Fatalf("trusted finding resolution failed: %+v %v", result, err)
	}
	outputs[0].Output = `{"verdict":"fail","summary":"unknown resolution","findings":[{"id":"auth","severity":"p1","body":"Missing check","status":"unresolved"}]}`
	result, _, err = plan.Aggregate(outputs)
	if err != nil || result.Verdict != VerdictFail || result.Findings[0].Status != "unresolved" || result.Findings[0].Body != "Missing check" {
		t.Fatalf("later chunk suppressed prior unresolved finding: %+v %v", result, err)
	}
	outputs[0].Output = `{"verdict":"pass","summary":"fixed","findings":[]}`
	snapshot.Previous.Findings = nil
	if _, _, err := plan.Aggregate(outputs); err == nil {
		t.Fatal("accepted omitted prior finding")
	}
}
