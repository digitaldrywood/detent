package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	defaultLogsWindow      = 24 * time.Hour
	defaultLogsLimit       = 1000
	defaultLogsMaxBytes    = int64(8 << 20)
	logsLineBufferSize     = 64 << 10
	logsMaxLineBytes       = 1 << 20
	logsMaxDiagnostics     = 100
	logsOutputJSON         = "json"
	logsOutputJSONL        = "jsonl"
	logsTruncationBytes    = "bytes"
	logsTruncationRecords  = "records"
	logsDiagnosticSeverity = "warning"
)

type logsFilter struct {
	ProjectID       string
	IssueID         string
	IssueIdentifier string
	WorkAttemptID   int64
	DetentSessionID int64
	ProviderSession string
	Since           time.Time
	Until           time.Time
	MinimumLevel    slog.Level
	MinimumLevelSet bool
	Limit           int
	MaxBytes        int64
}

type logsResult struct {
	Records []json.RawMessage `json:"records"`
	Summary logsSummary       `json:"summary"`
}

type logsSummary struct {
	MatchedRecords    int      `json:"matched_records"`
	ReturnedRecords   int      `json:"returned_records"`
	ScannedRecords    int      `json:"scanned_records"`
	ScannedBytes      int64    `json:"scanned_bytes"`
	MalformedRecords  int      `json:"malformed_records"`
	SuppressedDetails int      `json:"suppressed_diagnostics,omitempty"`
	Since             string   `json:"since"`
	Until             string   `json:"until"`
	Truncated         bool     `json:"truncated"`
	TruncationReasons []string `json:"truncation_reasons,omitempty"`
}

type logsDiagnostic struct {
	Severity string   `json:"severity"`
	Code     string   `json:"code"`
	Source   string   `json:"source,omitempty"`
	Record   int      `json:"record,omitempty"`
	Count    int      `json:"count,omitempty"`
	Reasons  []string `json:"reasons,omitempty"`
}

type logsSnapshotFile struct {
	name   string
	file   *os.File
	info   os.FileInfo
	active bool
}

type logsSegment struct {
	source *logsSnapshotFile
	offset int64
	size   int64
}

type logsCollector struct {
	filter      logsFilter
	result      logsResult
	diagnostics []logsDiagnostic
}

type logsCommandOptions struct {
	ProjectID       string
	IssueID         string
	IssueIdentifier string
	WorkAttemptID   int64
	DetentSessionID int64
	ProviderSession string
	Since           string
	Until           string
	Level           string
	Limit           int
	MaxBytes        int64
	Output          string
}

func newLogsCommand(configPath *string, opts options) *cobra.Command {
	values := logsCommandOptions{
		Limit:    defaultLogsLimit,
		MaxBytes: defaultLogsMaxBytes,
		Output:   logsOutputJSON,
	}
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Filter bounded runtime logs",
		Long: "Filter the active Detent dashboard JSON log and its numbered backups. " +
			"Filters combine conjunctively and results remain in chronological order.",
		Example: `detent logs --project-id detent --issue-identifier digitaldrywood/detent#1646 --output jsonl`,
		Args:    NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			filter, err := parseLogsFilter(values, time.Now())
			if err != nil {
				return err
			}
			path, maxBackups, err := resolveLogsSource(derefString(configPath), opts)
			if err != nil {
				return err
			}
			result, diagnostics, err := filterLogs(path, maxBackups, filter)
			if err != nil {
				return err
			}
			if err := writeLogsDiagnostics(cmd.ErrOrStderr(), diagnostics, result.Summary); err != nil {
				return fmt.Errorf("write log diagnostics: %w", err)
			}
			return writeLogsOutput(cmd.OutOrStdout(), values.Output, result)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&values.ProjectID, "project-id", "", "match project_id exactly")
	flags.StringVar(&values.IssueID, "issue-id", "", "match issue_id exactly")
	flags.StringVar(&values.IssueIdentifier, "issue-identifier", "", "match issue_identifier exactly")
	flags.Int64Var(&values.WorkAttemptID, "work-attempt-id", 0, "match work_attempt_id exactly")
	flags.Int64Var(&values.DetentSessionID, "detent-session-id", 0, "match detent_session_id exactly")
	flags.StringVar(&values.ProviderSession, "provider-session-id", "", "match provider_session_id exactly")
	flags.StringVar(&values.Since, "since", "", "inclusive RFC3339 start time (default: 24 hours ago)")
	flags.StringVar(&values.Until, "until", "", "inclusive RFC3339 end time (default: now)")
	flags.StringVar(&values.Level, "level", "", "minimum level: debug, info, warn, or error")
	flags.IntVar(&values.Limit, "limit", defaultLogsLimit, "maximum matching records")
	flags.Int64Var(&values.MaxBytes, "max-bytes", defaultLogsMaxBytes, "maximum source bytes scanned")
	flags.StringVar(&values.Output, "output", logsOutputJSON, "output shape: json or jsonl")
	return cmd
}

func parseLogsFilter(values logsCommandOptions, now time.Time) (logsFilter, error) {
	if values.Limit <= 0 {
		return logsFilter{}, ValidationError("--limit must be greater than 0")
	}
	if values.MaxBytes <= 0 {
		return logsFilter{}, ValidationError("--max-bytes must be greater than 0")
	}
	values.Output = strings.ToLower(strings.TrimSpace(values.Output))
	if values.Output != logsOutputJSON && values.Output != logsOutputJSONL {
		return logsFilter{}, ValidationError("--output must be json or jsonl")
	}
	now = now.UTC()
	since, err := parseLogsTime(values.Since, now.Add(-defaultLogsWindow), "--since")
	if err != nil {
		return logsFilter{}, err
	}
	until, err := parseLogsTime(values.Until, now, "--until")
	if err != nil {
		return logsFilter{}, err
	}
	if since.After(until) {
		return logsFilter{}, ValidationError("--since must be before or equal to --until")
	}
	minimumLevel, minimumLevelSet, err := parseLogsLevel(values.Level)
	if err != nil {
		return logsFilter{}, err
	}
	if values.WorkAttemptID < 0 {
		return logsFilter{}, ValidationError("--work-attempt-id must be greater than or equal to 0")
	}
	if values.DetentSessionID < 0 {
		return logsFilter{}, ValidationError("--detent-session-id must be greater than or equal to 0")
	}
	return logsFilter{
		ProjectID:       strings.TrimSpace(values.ProjectID),
		IssueID:         strings.TrimSpace(values.IssueID),
		IssueIdentifier: strings.TrimSpace(values.IssueIdentifier),
		WorkAttemptID:   values.WorkAttemptID,
		DetentSessionID: values.DetentSessionID,
		ProviderSession: strings.TrimSpace(values.ProviderSession),
		Since:           since,
		Until:           until,
		MinimumLevel:    minimumLevel,
		MinimumLevelSet: minimumLevelSet,
		Limit:           values.Limit,
		MaxBytes:        values.MaxBytes,
	}, nil
}

func parseLogsTime(value string, fallback time.Time, flag string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback.UTC(), nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, ValidationError(flag + " must be an RFC3339 timestamp")
	}
	return parsed.UTC(), nil
}

func parseLogsLevel(value string) (slog.Level, bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return 0, false, nil
	case "debug":
		return slog.LevelDebug, true, nil
	case "info":
		return slog.LevelInfo, true, nil
	case "warn", "warning":
		return slog.LevelWarn, true, nil
	case "error":
		return slog.LevelError, true, nil
	default:
		return 0, false, ValidationError("--level must be debug, info, warn, or error")
	}
}

func resolveLogsSource(configPath string, opts options) (string, int, error) {
	resolution, err := resolveConfigPathResolution(configPath, opts)
	if err != nil {
		return "", 0, err
	}
	read := opts.readDoctor
	if read == nil {
		read = func(path string) (globalconfig.Config, error) {
			return globalconfig.Read(path, globalconfig.WithMissingWorkflowFiles())
		}
	}
	cfg, err := read(resolution.Path)
	if err != nil {
		if !missingGlobalConfig(err) {
			return "", 0, err
		}
		cfg, err = globalconfig.DefaultAt(resolution.Path, globalconfig.WithMissingWorkflowFiles())
		if err != nil {
			return "", 0, err
		}
	}
	maxBackups := defaultRuntimeLogMaxBackups
	if cfg.LogMaxBackups != nil {
		maxBackups = max(0, *cfg.LogMaxBackups)
	}
	path, backups := logsSourceFromBoot(BootConfig{
		Global: cfg,
		Runtime: RuntimeSettings{
			LogMaxBackups: RuntimeIntValue{Value: maxBackups},
		},
	})
	return path, backups, nil
}

func logsSourceFromBoot(cfg BootConfig) (string, int) {
	return runtimeLogPath(cfg), max(0, cfg.Runtime.LogMaxBackups.Value)
}

func filterLogs(path string, maxBackups int, filter logsFilter) (logsResult, []logsDiagnostic, error) {
	files, err := openLogsSnapshot(path, maxBackups)
	if err != nil {
		return logsResult{}, nil, err
	}
	if len(files) == 0 {
		return logsResult{}, nil, NewClassifiedError(
			os.ErrNotExist,
			errorCodeGeneral,
			"runtime log file is unavailable",
			"File logs are written only in terminal-dashboard mode; headless and text modes write to stdout or stderr.",
			nil,
		)
	}
	result, diagnostics := filterLogsSnapshot(files, filter)
	if err := closeLogsSnapshot(files); err != nil {
		return logsResult{}, diagnostics, fmt.Errorf("close runtime log snapshot: %w", err)
	}
	return result, diagnostics, nil
}

func filterLogsSnapshot(files []*logsSnapshotFile, filter logsFilter) (logsResult, []logsDiagnostic) {
	segments, scannedBytes, byteTruncated := boundedLogsSegments(files, filter.MaxBytes)
	collector := logsCollector{
		filter: filter,
		result: logsResult{
			Records: make([]json.RawMessage, 0),
			Summary: logsSummary{
				ScannedBytes: scannedBytes,
				Since:        filter.Since.Format(time.RFC3339Nano),
				Until:        filter.Until.Format(time.RFC3339Nano),
			},
		},
	}
	if byteTruncated {
		collector.truncate(logsTruncationBytes)
	}
	for _, segment := range segments {
		collector.scan(segment)
	}
	collector.result.Summary.ReturnedRecords = len(collector.result.Records)
	collector.result.Summary.SuppressedDetails = max(0, collector.result.Summary.MalformedRecords-len(collector.diagnostics))
	return collector.result, collector.diagnostics
}

func openLogsSnapshot(path string, maxBackups int) ([]*logsSnapshotFile, error) {
	paths := make([]string, 0, maxBackups+1)
	for index := maxBackups; index >= 1; index-- {
		paths = append(paths, rotatedLogPath(path, index))
	}
	paths = append(paths, path)
	files := make([]*logsSnapshotFile, 0, len(paths))
	for _, candidate := range paths {
		file, err := openLogsFile(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, errors.Join(fmt.Errorf("open runtime log: %w", err), closeLogsSnapshot(files))
		}
		info, err := file.Stat()
		if err != nil {
			return nil, errors.Join(fmt.Errorf("inspect runtime log: %w", err), file.Close(), closeLogsSnapshot(files))
		}
		if slices.ContainsFunc(files, func(existing *logsSnapshotFile) bool {
			return os.SameFile(existing.info, info)
		}) {
			if err := file.Close(); err != nil {
				return nil, errors.Join(fmt.Errorf("close duplicate runtime log: %w", err), closeLogsSnapshot(files))
			}
			continue
		}
		files = append(files, &logsSnapshotFile{
			name:   filepath.Base(candidate),
			file:   file,
			info:   info,
			active: candidate == path,
		})
	}
	return files, nil
}

func closeLogsSnapshot(files []*logsSnapshotFile) error {
	var result error
	for _, source := range files {
		if source != nil && source.file != nil {
			result = errors.Join(result, source.file.Close())
		}
	}
	return result
}

func boundedLogsSegments(files []*logsSnapshotFile, maxBytes int64) ([]logsSegment, int64, bool) {
	remaining := maxBytes
	segments := make([]logsSegment, 0, len(files))
	var scanned int64
	truncated := false
	for index := len(files) - 1; index >= 0; index-- {
		size := max(int64(0), files[index].info.Size())
		if remaining <= 0 {
			if size > 0 {
				truncated = true
			}
			continue
		}
		take := min(size, remaining)
		if take < size {
			truncated = true
		}
		segments = append(segments, logsSegment{source: files[index], offset: size - take, size: take})
		remaining -= take
		scanned += take
	}
	slices.Reverse(segments)
	return segments, scanned, truncated
}

func (c *logsCollector) scan(segment logsSegment) {
	if segment.size <= 0 {
		return
	}
	reader := bufio.NewReaderSize(io.NewSectionReader(segment.source.file, segment.offset, segment.size), logsLineBufferSize)
	if logsSegmentStartsMidLine(segment) {
		_, _, _, err := readLogsLine(reader)
		if err != nil {
			return
		}
	}
	for record := 1; ; record++ {
		raw, complete, tooLong, err := readLogsLine(reader)
		if len(raw) == 0 && errors.Is(err, io.EOF) {
			return
		}
		if tooLong {
			c.malformed(segment.source.name, record, "line_too_long")
		} else if len(bytes.TrimSpace(raw)) > 0 {
			if !complete && segment.source.active {
				if json.Valid(bytes.TrimSpace(raw)) {
					c.scanRecord(segment.source.name, record, bytes.TrimSpace(raw))
				} else {
					c.malformed(segment.source.name, record, "partial_final_line")
				}
			} else {
				c.scanRecord(segment.source.name, record, bytes.TrimSpace(raw))
			}
		}
		if err != nil {
			return
		}
	}
}

func logsSegmentStartsMidLine(segment logsSegment) bool {
	if segment.offset <= 0 {
		return false
	}
	var previous [1]byte
	n, err := segment.source.file.ReadAt(previous[:], segment.offset-1)
	return err != nil || n != 1 || previous[0] != '\n'
}

func readLogsLine(reader *bufio.Reader) ([]byte, bool, bool, error) {
	var line []byte
	tooLong := false
	for {
		fragment, err := reader.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(fragment) <= logsMaxLineBytes {
				line = append(line, fragment...)
			} else {
				line = nil
				tooLong = true
			}
		}
		switch {
		case err == nil:
			return line, true, tooLong, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return line, false, tooLong, io.EOF
		default:
			return line, false, tooLong, err
		}
	}
}

func (c *logsCollector) scanRecord(source string, record int, raw []byte) {
	c.result.Summary.ScannedRecords++
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		c.malformed(source, record, "malformed_json")
		return
	}
	matches, code := logsRecordMatches(fields, c.filter)
	if code != "" {
		c.malformed(source, record, code)
		return
	}
	if !matches {
		return
	}
	c.result.Summary.MatchedRecords++
	copyOfRaw := append(json.RawMessage(nil), raw...)
	if len(c.result.Records) == c.filter.Limit {
		copy(c.result.Records, c.result.Records[1:])
		c.result.Records[len(c.result.Records)-1] = copyOfRaw
		c.truncate(logsTruncationRecords)
		return
	}
	c.result.Records = append(c.result.Records, copyOfRaw)
}

func logsRecordMatches(fields map[string]json.RawMessage, filter logsFilter) (bool, string) {
	timestamp, ok := logsString(fields, "time")
	if !ok {
		return false, "invalid_time"
	}
	recordedAt, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return false, "invalid_time"
	}
	recordedAt = recordedAt.UTC()
	if recordedAt.Before(filter.Since) || recordedAt.After(filter.Until) {
		return false, ""
	}
	for _, match := range []struct {
		key  string
		want string
	}{
		{telemetry.ProjectIDKey, filter.ProjectID},
		{telemetry.IssueIDKey, filter.IssueID},
		{telemetry.IssueIdentifierKey, filter.IssueIdentifier},
		{telemetry.ProviderSessionIDKey, filter.ProviderSession},
	} {
		if match.want == "" {
			continue
		}
		if _, exists := fields[match.key]; !exists {
			return false, ""
		}
		got, valid := logsString(fields, match.key)
		if !valid {
			return false, "invalid_" + match.key
		}
		if got != match.want {
			return false, ""
		}
	}
	for _, match := range []struct {
		key  string
		want int64
	}{
		{telemetry.WorkAttemptIDKey, filter.WorkAttemptID},
		{telemetry.DetentSessionIDKey, filter.DetentSessionID},
	} {
		if match.want == 0 {
			continue
		}
		if _, exists := fields[match.key]; !exists {
			return false, ""
		}
		got, valid := logsInt64(fields, match.key)
		if !valid {
			return false, "invalid_" + match.key
		}
		if got != match.want {
			return false, ""
		}
	}
	if filter.MinimumLevelSet {
		level, valid := logsRecordLevel(fields)
		if !valid {
			return false, "invalid_level"
		}
		if level < filter.MinimumLevel {
			return false, ""
		}
	}
	return true, ""
}

func logsString(fields map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := fields[key]
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func logsInt64(fields map[string]json.RawMessage, key string) (int64, bool) {
	raw, ok := fields[key]
	if !ok {
		return 0, false
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	return value, true
}

func logsRecordLevel(fields map[string]json.RawMessage) (slog.Level, bool) {
	value, ok := logsString(fields, "level")
	if !ok {
		return 0, false
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToUpper(strings.TrimSpace(value)))); err != nil {
		return 0, false
	}
	return level, true
}

func (c *logsCollector) malformed(source string, record int, code string) {
	c.result.Summary.MalformedRecords++
	if len(c.diagnostics) < logsMaxDiagnostics {
		c.diagnostics = append(c.diagnostics, logsDiagnostic{
			Severity: logsDiagnosticSeverity,
			Code:     code,
			Source:   source,
			Record:   record,
		})
	}
}

func (c *logsCollector) truncate(reason string) {
	if slices.Contains(c.result.Summary.TruncationReasons, reason) {
		return
	}
	c.result.Summary.Truncated = true
	c.result.Summary.TruncationReasons = append(c.result.Summary.TruncationReasons, reason)
}

func writeLogsOutput(out io.Writer, output string, result logsResult) error {
	if out == nil {
		out = io.Discard
	}
	switch strings.ToLower(strings.TrimSpace(output)) {
	case logsOutputJSON:
		return WriteJSON(out, result)
	case logsOutputJSONL:
		for _, record := range result.Records {
			line := append([]byte(nil), record...)
			line = append(line, '\n')
			if _, err := out.Write(line); err != nil {
				return err
			}
		}
		return nil
	default:
		return ValidationError("--output must be json or jsonl")
	}
}

func writeLogsDiagnostics(out io.Writer, diagnostics []logsDiagnostic, summary logsSummary) error {
	if out == nil {
		out = io.Discard
	}
	encoder := json.NewEncoder(out)
	for _, diagnostic := range diagnostics {
		if err := encoder.Encode(diagnostic); err != nil {
			return err
		}
	}
	if summary.SuppressedDetails > 0 {
		if err := encoder.Encode(logsDiagnostic{
			Severity: logsDiagnosticSeverity,
			Code:     "diagnostics_suppressed",
			Count:    summary.SuppressedDetails,
		}); err != nil {
			return err
		}
	}
	if summary.Truncated {
		return encoder.Encode(logsDiagnostic{
			Severity: logsDiagnosticSeverity,
			Code:     "output_truncated",
			Reasons:  summary.TruncationReasons,
		})
	}
	return nil
}
