package tracker

import (
	"regexp"
	"strings"

	"github.com/digitaldrywood/detent/internal/runtimeoutput"
)

var nativeFinalizationPrivateText = regexp.MustCompile(`(?i)(?:https?://|file://|[a-z]:[\\/]|~/|/|[a-z0-9_.-]+/)[^\s"'<>]+|[a-z0-9_]*(?:token|secret|password|api_key)[a-z0-9_]*\s*[=:]\s*[^\s,;]+`)

func (f NativeFinalization) Public() NativeFinalization {
	f.Source = "host_native_change"
	f.Coverage = "recorded_change_publication_result_only; settled_is_not_issue_acceptance; historical_local_metadata_not_backfilled"
	f.Unavailable = []string{}
	if f.SourceVersion == nil {
		f.Unavailable = append(f.Unavailable, "source_version")
	}
	if f.SourceAttemptID == "" {
		f.Unavailable = append(f.Unavailable, "source_attempt")
	}
	f.VersionError = f.publicText(f.VersionError)
	f.Error = f.publicText(f.Error)
	return f
}

func (f *NativeFinalization) publicText(value string) string {
	value = strings.ToValidUTF8(value, "�")
	if first, _, multiline := strings.Cut(value, "\n"); multiline {
		value = first + " [private output omitted]"
		f.TextRedacted = true
	}
	public := nativeFinalizationPrivateText.ReplaceAllString(value, "[redacted]")
	f.TextRedacted = f.TextRedacted || public != value
	text := runtimeoutput.Truncate(public, NativeFinalizationTextLimit)
	f.TextTruncated = f.TextTruncated || text.Truncation != nil
	return text.Value
}
