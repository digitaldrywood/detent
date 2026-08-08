package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestParseLogsFilter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		values      logsCommandOptions
		wantSince   time.Time
		wantUntil   time.Time
		wantLevel   bool
		wantErrText string
	}{
		{
			name:      "default bounded window",
			values:    logsCommandOptions{Limit: defaultLogsLimit, MaxBytes: defaultLogsMaxBytes, Output: logsOutputJSON},
			wantSince: now.Add(-defaultLogsWindow),
			wantUntil: now,
		},
		{
			name: "normalizes explicit boundaries to UTC",
			values: logsCommandOptions{
				Since: "2026-08-07T06:00:00-05:00", Until: "2026-08-07T07:00:00-05:00",
				Level: "warning", Limit: 2, MaxBytes: 1024, Output: logsOutputJSONL,
			},
			wantSince: time.Date(2026, time.August, 7, 11, 0, 0, 0, time.UTC),
			wantUntil: time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC),
			wantLevel: true,
		},
		{name: "rejects zero limit", values: logsCommandOptions{MaxBytes: 1, Output: logsOutputJSON}, wantErrText: "--limit must be greater than 0"},
		{name: "rejects zero byte bound", values: logsCommandOptions{Limit: 1, Output: logsOutputJSON}, wantErrText: "--max-bytes must be greater than 0"},
		{name: "rejects output", values: logsCommandOptions{Limit: 1, MaxBytes: 1, Output: "pretty"}, wantErrText: "--output must be json or jsonl"},
		{name: "rejects start syntax", values: logsCommandOptions{Since: "yesterday", Limit: 1, MaxBytes: 1, Output: logsOutputJSON}, wantErrText: "--since must be an RFC3339 timestamp"},
		{name: "rejects end before start", values: logsCommandOptions{Since: now.Format(time.RFC3339), Until: now.Add(-time.Second).Format(time.RFC3339), Limit: 1, MaxBytes: 1, Output: logsOutputJSON}, wantErrText: "--since must be before or equal to --until"},
		{name: "rejects level", values: logsCommandOptions{Level: "notice", Limit: 1, MaxBytes: 1, Output: logsOutputJSON}, wantErrText: "--level must be debug, info, warn, or error"},
		{name: "rejects negative attempt", values: logsCommandOptions{WorkAttemptID: -1, Limit: 1, MaxBytes: 1, Output: logsOutputJSON}, wantErrText: "--work-attempt-id must be greater than or equal to 0"},
		{name: "rejects negative session", values: logsCommandOptions{DetentSessionID: -1, Limit: 1, MaxBytes: 1, Output: logsOutputJSON}, wantErrText: "--detent-session-id must be greater than or equal to 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseLogsFilter(tt.values, now)
			if tt.wantErrText != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrText) {
					t.Fatalf("parseLogsFilter() error = %v, want containing %q", err, tt.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLogsFilter() error = %v", err)
			}
			if !got.Since.Equal(tt.wantSince) || !got.Until.Equal(tt.wantUntil) {
				t.Fatalf("window = %s..%s, want %s..%s", got.Since, got.Until, tt.wantSince, tt.wantUntil)
			}
			if got.MinimumLevelSet != tt.wantLevel {
				t.Fatalf("MinimumLevelSet = %t, want %t", got.MinimumLevelSet, tt.wantLevel)
			}
		})
	}
}

func TestFilterLogsCombinesFiltersAndOrdersLevels(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "detent.log")
	base := time.Date(2026, time.August, 7, 10, 0, 0, 0, time.UTC)
	records := []string{
		testLogRecord(base, "DEBUG", "debug", "alpha", "1", "owner/repo#1", 10, 20, "provider-a"),
		testLogRecord(base.Add(time.Minute), "INFO", "info", "alpha", "1", "owner/repo#1", 10, 20, "provider-a"),
		testLogRecord(base.Add(2*time.Minute), "WARN", "warn", "alpha", "1", "owner/repo#1", 10, 20, "provider-a"),
		testLogRecord(base.Add(3*time.Minute), "ERROR", "error", "beta", "2", "owner/repo#2", 11, 21, "provider-b"),
	}
	writeLogLines(t, path, records...)
	tests := []struct {
		name   string
		filter logsFilter
		want   []string
	}{
		{
			name:   "UTC boundaries are inclusive",
			filter: testLogsFilter(base.Add(time.Minute), base.Add(2*time.Minute)),
			want:   []string{"info", "warn"},
		},
		{
			name: "canonical filters combine conjunctively",
			filter: func() logsFilter {
				filter := testLogsFilter(base, base.Add(4*time.Minute))
				filter.ProjectID = "alpha"
				filter.IssueID = "1"
				filter.IssueIdentifier = "owner/repo#1"
				filter.WorkAttemptID = 10
				filter.DetentSessionID = 20
				filter.ProviderSession = "provider-a"
				filter.MinimumLevel, filter.MinimumLevelSet, _ = parseLogsLevel("warn")
				return filter
			}(),
			want: []string{"warn"},
		},
		{
			name: "project scope is exact",
			filter: func() logsFilter {
				filter := testLogsFilter(base, base.Add(4*time.Minute))
				filter.ProjectID = "beta"
				return filter
			}(),
			want: []string{"error"},
		},
	}
	for _, level := range []struct {
		name string
		want []string
	}{
		{name: "debug", want: []string{"debug", "info", "warn", "error"}},
		{name: "info", want: []string{"info", "warn", "error"}},
		{name: "warn", want: []string{"warn", "error"}},
		{name: "error", want: []string{"error"}},
	} {
		filter := testLogsFilter(base, base.Add(4*time.Minute))
		filter.MinimumLevel, filter.MinimumLevelSet, _ = parseLogsLevel(level.name)
		tests = append(tests, struct {
			name   string
			filter logsFilter
			want   []string
		}{name: "minimum level " + level.name, filter: filter, want: level.want})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, diagnostics, err := filterLogs(path, 0, tt.filter)
			if err != nil {
				t.Fatalf("filterLogs() error = %v", err)
			}
			if len(diagnostics) != 0 {
				t.Fatalf("diagnostics = %#v, want none", diagnostics)
			}
			if got := logMessages(t, result.Records); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("messages = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFilterLogsReadsRotationAndReportsMalformedRecordsSafely(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "detent.log")
	base := time.Date(2026, time.August, 7, 10, 0, 0, 0, time.UTC)
	writeLogLines(t, path+".2", testLogRecord(base, "INFO", "oldest", "alpha", "1", "owner/repo#1", 0, 0, ""))
	active := testLogRecord(base.Add(time.Minute), "INFO", "newest", "alpha", "1", "owner/repo#1", 0, 0, "") + "\n" +
		`{"time":"2026-08-07T10:02:00Z","secret":"DO-NOT-ECHO",}` + "\n" +
		`{"time":"2026-08-07T10:02:30Z","level":"INFO","project_id":42,"secret":"WRONG-TYPE"}` + "\n" +
		`{"time":"2026-08-07T10:03:00Z","secret":"ALSO-SECRET"`
	if err := os.WriteFile(path, []byte(active), 0o600); err != nil {
		t.Fatal(err)
	}

	filter := testLogsFilter(base.Add(-time.Hour), base.Add(time.Hour))
	filter.ProjectID = "alpha"
	result, diagnostics, err := filterLogs(path, 2, filter)
	if err != nil {
		t.Fatalf("filterLogs() error = %v", err)
	}
	if got, want := logMessages(t, result.Records), []string{"oldest", "newest"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}
	if result.Summary.MalformedRecords != 3 {
		t.Fatalf("MalformedRecords = %d, want 3", result.Summary.MalformedRecords)
	}
	if got, want := []string{diagnostics[0].Code, diagnostics[1].Code, diagnostics[2].Code}, []string{"malformed_json", "invalid_project_id", "partial_final_line"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("diagnostic codes = %v, want %v", got, want)
	}
	var stderr bytes.Buffer
	if err := writeLogsDiagnostics(&stderr, diagnostics, result.Summary); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr.String(), "DO-NOT-ECHO") || strings.Contains(stderr.String(), "WRONG-TYPE") || strings.Contains(stderr.String(), "ALSO-SECRET") {
		t.Fatalf("diagnostics leaked record contents: %s", stderr.String())
	}
}

func TestOpenLogsSnapshotRetriesConcurrentRotationAndDeduplicates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "detent.log")
	base := time.Date(2026, time.August, 7, 10, 0, 0, 0, time.UTC)
	writeLogLines(t, path+".1", testLogRecord(base, "INFO", "backup", "alpha", "1", "", 0, 0, ""))
	writeLogLines(t, path, testLogRecord(base.Add(time.Minute), "INFO", "active", "alpha", "1", "", 0, 0, ""))
	attempts := 0
	files, err := openLogsSnapshotWithHook(logsSnapshotPaths(path, 2), func(attempt int) error {
		attempts++
		if attempt > 0 {
			return nil
		}
		if err := rotateLogFiles(path, 2); err != nil {
			return err
		}
		writeLogLines(t, path, testLogRecord(base.Add(2*time.Minute), "INFO", "after rotation", "alpha", "1", "", 0, 0, ""))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closeLogsSnapshot(files); err != nil {
			t.Errorf("closeLogsSnapshot() error = %v", err)
		}
	})
	if attempts != 2 {
		t.Fatalf("snapshot attempts = %d, want 2", attempts)
	}
	result, diagnostics := filterLogsSnapshot(files, testLogsFilter(base.Add(-time.Hour), base.Add(time.Hour)))
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v, want none", diagnostics)
	}
	if got, want := logMessages(t, result.Records), []string{"backup", "active", "after rotation"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot messages = %v, want %v", got, want)
	}

	linked := filepath.Join(dir, "linked.log")
	writeLogLines(t, linked, testLogRecord(base, "INFO", "once", "alpha", "1", "", 0, 0, ""))
	if err := os.Link(linked, linked+".1"); err != nil {
		t.Fatal(err)
	}
	deduplicated, err := openLogsSnapshot(linked, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closeLogsSnapshot(deduplicated); err != nil {
			t.Errorf("closeLogsSnapshot() error = %v", err)
		}
	})
	if len(deduplicated) != 1 {
		t.Fatalf("snapshot file count = %d, want 1", len(deduplicated))
	}
}

func TestFilterLogsEnforcesByteAndRecordBounds(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, time.August, 7, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name         string
		lines        []string
		filter       logsFilter
		wantMessages []string
		wantMatched  int
		wantReason   string
	}{
		{
			name: "byte tail starts on record boundary",
			lines: []string{
				testLogRecord(base, "INFO", "old", "alpha", "1", "", 0, 0, ""),
				testLogRecord(base.Add(time.Minute), "INFO", "new", "alpha", "1", "", 0, 0, ""),
			},
			filter: func() logsFilter {
				filter := testLogsFilter(base.Add(-time.Hour), base.Add(time.Hour))
				filter.MaxBytes = int64(len(testLogRecord(base.Add(time.Minute), "INFO", "new", "alpha", "1", "", 0, 0, "")) + 1)
				return filter
			}(),
			wantMessages: []string{"new"},
			wantMatched:  1,
			wantReason:   logsTruncationBytes,
		},
		{
			name: "record limit retains latest matches in order",
			lines: []string{
				testLogRecord(base, "INFO", "one", "alpha", "1", "", 0, 0, ""),
				testLogRecord(base.Add(time.Minute), "INFO", "two", "alpha", "1", "", 0, 0, ""),
				testLogRecord(base.Add(2*time.Minute), "INFO", "three", "alpha", "1", "", 0, 0, ""),
			},
			filter: func() logsFilter {
				filter := testLogsFilter(base.Add(-time.Hour), base.Add(time.Hour))
				filter.Limit = 2
				return filter
			}(),
			wantMessages: []string{"two", "three"},
			wantMatched:  3,
			wantReason:   logsTruncationRecords,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "detent.log")
			writeLogLines(t, path, tt.lines...)
			result, diagnostics, err := filterLogs(path, 0, tt.filter)
			if err != nil {
				t.Fatal(err)
			}
			if got := logMessages(t, result.Records); !reflect.DeepEqual(got, tt.wantMessages) {
				t.Fatalf("messages = %v, want %v", got, tt.wantMessages)
			}
			if result.Summary.MatchedRecords != tt.wantMatched || result.Summary.ReturnedRecords != len(tt.wantMessages) {
				t.Fatalf("summary counts = matched %d returned %d", result.Summary.MatchedRecords, result.Summary.ReturnedRecords)
			}
			if !result.Summary.Truncated || !reflect.DeepEqual(result.Summary.TruncationReasons, []string{tt.wantReason}) {
				t.Fatalf("truncation = %t %v, want %q", result.Summary.Truncated, result.Summary.TruncationReasons, tt.wantReason)
			}
			var stderr bytes.Buffer
			if err := writeLogsDiagnostics(&stderr, diagnostics, result.Summary); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stderr.String(), `"code":"output_truncated"`) {
				t.Fatalf("stderr does not signal truncation: %s", stderr.String())
			}
		})
	}
}

func TestWriteLogsOutputShapes(t *testing.T) {
	t.Parallel()
	record := json.RawMessage(`{"time":"2026-08-07T10:00:00Z","level":"INFO","msg":"ready"}`)
	result := logsResult{Records: []json.RawMessage{record}, Summary: logsSummary{ReturnedRecords: 1}}
	tests := []struct {
		name   string
		output string
		check  func(*testing.T, []byte)
	}{
		{
			name:   "JSON envelope",
			output: logsOutputJSON,
			check: func(t *testing.T, raw []byte) {
				var decoded struct {
					Records []json.RawMessage `json:"records"`
					Summary logsSummary       `json:"summary"`
				}
				if err := json.Unmarshal(raw, &decoded); err != nil {
					t.Fatal(err)
				}
				if len(decoded.Records) != 1 || decoded.Summary.ReturnedRecords != 1 {
					t.Fatalf("decoded output = %#v", decoded)
				}
			},
		},
		{
			name:   "JSONL raw records",
			output: logsOutputJSONL,
			check: func(t *testing.T, raw []byte) {
				if got, want := string(raw), string(record)+"\n"; got != want {
					t.Fatalf("output = %q, want %q", got, want)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			if err := writeLogsOutput(&output, tt.output, result); err != nil {
				t.Fatal(err)
			}
			tt.check(t, output.Bytes())
		})
	}
}

func TestLogsSourceUsesRuntimeResolutionAndOverride(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "global.yaml")
	backups := 3
	opts := defaultOptions()
	opts.resolvePath = func(string) (globalconfig.PathResolution, error) {
		return globalconfig.PathResolution{Path: configPath, Rule: globalconfig.PathRuleFlag}, nil
	}
	opts.readDoctor = func(string) (globalconfig.Config, error) {
		return globalconfig.Config{Path: configPath, LogMaxBackups: &backups}, nil
	}
	path, gotBackups, err := resolveLogsSource("ignored", opts)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "detent.log"); path != want || gotBackups != backups {
		t.Fatalf("source = %q, %d, want %q, %d", path, gotBackups, want, backups)
	}

	override := filepath.Join(dir, "runtime", "operator.jsonl")
	path, gotBackups = logsSourceFromBoot(BootConfig{
		RuntimeDBPath:  filepath.Join(dir, "runtime", "state.db"),
		RuntimeLogPath: override,
		Runtime:        RuntimeSettings{LogMaxBackups: RuntimeIntValue{Value: 7}},
	})
	if path != override || gotBackups != 7 {
		t.Fatalf("overridden source = %q, %d, want %q, 7", path, gotBackups, override)
	}
}

func TestFilterLogsExplainsNoFileLoggingModes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "detent.log")
	_, _, err := filterLogs(path, 5, testLogsFilter(time.Unix(0, 0), time.Now().Add(time.Hour)))
	if err == nil {
		t.Fatal("filterLogs() error = nil")
	}
	for _, want := range []string{"runtime log file is unavailable", "terminal-dashboard mode", "headless and text modes"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
}

func TestFilterLogsBoundsMalformedDiagnostics(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "detent.log")
	lines := make([]string, logsMaxDiagnostics+2)
	for index := range lines {
		lines[index] = `{"secret":"NEVER-ECHO",}`
	}
	writeLogLines(t, path, lines...)
	result, diagnostics, err := filterLogs(path, 0, testLogsFilter(time.Unix(0, 0), time.Now().Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != logsMaxDiagnostics || result.Summary.SuppressedDetails != 2 {
		t.Fatalf("diagnostics = %d, suppressed = %d, want %d and 2", len(diagnostics), result.Summary.SuppressedDetails, logsMaxDiagnostics)
	}
	var stderr bytes.Buffer
	if err := writeLogsDiagnostics(&stderr, diagnostics, result.Summary); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr.String(), "NEVER-ECHO") || !strings.Contains(stderr.String(), `"code":"diagnostics_suppressed","count":2`) {
		t.Fatalf("bounded diagnostics = %s", stderr.String())
	}
}

func TestLogsCommandReadsResolvedRuntimeLog(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "global.yaml")
	path := filepath.Join(dir, "detent.log")
	at := time.Date(2026, time.August, 7, 10, 0, 0, 0, time.UTC)
	writeLogLines(t, path, testLogRecord(at, "INFO", "command", "alpha", "1", "owner/repo#1", 0, 0, ""))
	opts := defaultOptions()
	opts.resolvePath = func(string) (globalconfig.PathResolution, error) {
		return globalconfig.PathResolution{Path: configPath, Rule: globalconfig.PathRuleFlag}, nil
	}
	opts.readDoctor = func(string) (globalconfig.Config, error) {
		return globalconfig.Config{Path: configPath}, nil
	}
	cmd := NewRootCommand(t.Context(), func(configured *options) { *configured = opts })
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"logs",
		"--project-id", "alpha",
		"--since", at.Format(time.RFC3339),
		"--until", at.Format(time.RFC3339),
		"--output", "jsonl",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(stdout.String(), `"msg":"command"`) {
		t.Fatalf("stdout = %s", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %s, want empty", stderr.String())
	}
}

func testLogsFilter(since time.Time, until time.Time) logsFilter {
	return logsFilter{
		Since:    since.UTC(),
		Until:    until.UTC(),
		Limit:    100,
		MaxBytes: 1 << 20,
	}
}

func testLogRecord(at time.Time, level string, message string, projectID string, issueID string, issueIdentifier string, workAttemptID int64, detentSessionID int64, providerSessionID string) string {
	fields := map[string]any{
		"time":                at.Format(time.RFC3339Nano),
		"level":               level,
		"msg":                 message,
		"project_id":          projectID,
		"issue_id":            issueID,
		"issue_identifier":    issueIdentifier,
		"work_attempt_id":     workAttemptID,
		"detent_session_id":   detentSessionID,
		"provider_session_id": providerSessionID,
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func writeLogLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func logMessages(t *testing.T, records []json.RawMessage) []string {
	t.Helper()
	messages := make([]string, 0, len(records))
	for _, record := range records {
		var fields map[string]any
		if err := json.Unmarshal(record, &fields); err != nil {
			t.Fatal(err)
		}
		message, ok := fields["msg"].(string)
		if !ok {
			t.Fatalf("record has no string msg: %s", record)
		}
		messages = append(messages, message)
	}
	return messages
}

func TestLogsErrorsRemainClassified(t *testing.T) {
	t.Parallel()
	_, err := parseLogsFilter(logsCommandOptions{Limit: -1, MaxBytes: 1, Output: logsOutputJSON}, time.Now())
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("error = %v, want ErrValidation", err)
	}
	if got := ClassifyError(err).ExitCode; got != 3 {
		t.Fatalf("exit code = %d, want 3", got)
	}
}
