package tracker

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/runtimeoutput"
)

func (p NativePRPublication) EffectID(kind string) string {
	identity := strings.Join([]string{kind, p.ChangeID, p.VersionID, p.Repository, p.BaseRef, p.Branch, p.HeadSHA, p.PolicyID, p.SourceVersion.ChangeID, p.SourceVersion.VersionID, p.SourceVersion.HeadSHA, p.SourceAttemptID}, "\x00")
	if kind == "pr_create" {
		identity += "\x00" + p.External.Provider + "\x00" + p.External.ID + "\x00" + p.External.URL
	}
	digest := sha256.Sum256([]byte(identity))
	return "effect_" + hex.EncodeToString(digest[:16])
}

func (p NativePRPublication) Matches(change string, version ChangeVersion) bool {
	number, err := strconv.Atoi(p.External.ID)
	repository, parseErr := url.Parse(p.Repository)
	if parseErr != nil {
		return false
	}
	parts := strings.Split(strings.Trim(repository.Path, "/"), "/")
	return err == nil && number > 0 && strconv.Itoa(number) == p.External.ID && p.External.Provider == "github" &&
		repository.Scheme == "https" && repository.Host == "github.com" && repository.User == nil && repository.RawQuery == "" && repository.Fragment == "" &&
		len(parts) == 2 && parts[0] != "" && parts[1] != "" && parts[0] != "." && parts[1] != "." && parts[0] != ".." && parts[1] != ".." && repository.EscapedPath() == repository.Path &&
		!strings.ContainsAny(parts[0]+parts[1], " \t\r\n") && p.External.URL == p.Repository+"/pull/"+p.External.ID && (version.External == nil || *version.External == p.External) &&
		p.ChangeID == change && p.VersionID == version.ID && p.Repository == version.Repository && p.HeadSHA == version.HeadSHA && p.PolicyID == version.PolicyID &&
		p.BaseRef != "" && p.Branch != "" && p.SourceVersion.ChangeID == change && p.SourceVersion.VersionID == version.ID && p.SourceVersion.HeadSHA == version.HeadSHA && p.SourceAttemptID == version.AttemptID
}

var nativeFinalizationPrivateText = regexp.MustCompile(`(?i)(?:https?://|file://|[a-z]:[\\/]|~/|/|[a-z0-9_.-]+/)[^\s"'<>]+|[a-z0-9_]*(?:token|secret|password|api_key)[a-z0-9_]*\s*[=:]\s*[^\s,;]+`)
var nativeGateSecretField = regexp.MustCompile(`(?i)((?:"|')?(?:authorization|token|secret|password|credential|api[_-]?key)(?:"|')?\s*[:=]\s*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;]+)`)
var nativeGateBearer = regexp.MustCompile(`(?i)\bBearer\s+[^\s,;]+`)
var nativeGateToken = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-[A-Za-z0-9_-]{20,})\b`)
var nativeGatePrivateCommand = regexp.MustCompile(`(?i)(token|secret|password|credential|api[_-]?key|https?://|file://|(?:^|\s)(?:[a-z]:[\\/]|/|~/))`)

func PublicGateResult(result gate.CommandResult) gate.CommandResult {
	if nativeGatePrivateCommand.MatchString(result.Command) {
		result.Command = "[redacted]"
	}
	text, truncated := publicGateOutput(result.Output)
	result.Output = text
	result.OutputTruncated = result.OutputTruncated || truncated
	return result
}

func publicGateOutput(value string) (string, bool) {
	value = strings.ToValidUTF8(value, "�")
	value = strings.Map(func(r rune) rune {
		if r < ' ' && r != '\n' && r != '\r' && r != '\t' {
			return ' '
		}
		return r
	}, value)
	value = nativeGateSecretField.ReplaceAllString(value, `${1}[redacted]`)
	value = nativeGateBearer.ReplaceAllString(value, "Bearer [redacted]")
	value = nativeGateToken.ReplaceAllString(value, "[redacted]")
	value = nativeFinalizationPrivateText.ReplaceAllString(value, "[redacted]")
	const marker = "[earlier output truncated]\n"
	limit := NativeFinalizationTextLimit
	if len(value) <= limit {
		return value, false
	}
	suffixLimit := limit - len(marker)
	start := len(value) - suffixLimit
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return marker + value[start:], true
}

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
