package runtimeoutput

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

const (
	Marker           = "\n...[truncated]...\n"
	StrategyHeadTail = "head_tail"
	StrategyPrefix   = "prefix"
)

type Policy struct {
	MaxBytes int
}

type Text struct {
	Value      string
	Truncation *Truncation
}

type Truncation struct {
	Truncated     bool   `json:"truncated"`
	OriginalBytes int    `json:"original_bytes"`
	StoredBytes   int    `json:"stored_bytes"`
	LimitBytes    int    `json:"limit_bytes"`
	OmittedBytes  int    `json:"omitted_bytes,omitempty"`
	Strategy      string `json:"strategy,omitempty"`
}

type Buffer struct {
	policy        Policy
	full          strings.Builder
	originalBytes int
	truncated     bool
	head          string
	headSealed    bool
	tail          string
}

func NewBuffer(policy Policy) *Buffer {
	if policy.MaxBytes < 0 {
		policy.MaxBytes = 0
	}
	return &Buffer{policy: policy}
}

func Truncate(value string, maxBytes int) Text {
	buffer := NewBuffer(Policy{MaxBytes: maxBytes})
	buffer.Append(value)
	return buffer.Text()
}

var diagnosticFailure = regexp.MustCompile(`(?m)^[\t ]*(?:` +
	`panic:|fatal error:|--- FAIL:|FAIL(?:[\t ]|$)|WARNING: DATA RACE|AssertionError:|` +
	`MIGRATION ORDERING CONFLICT DETECTED!|g?make(?:\[[0-9]+\])?:[\t ]+\*\*\*|` +
	`[^\n]*check-fast-stage[^\n]*exit_status=[1-9][0-9]*(?:\b|$)|` +
	`(?i:[^\n]*\berror(?:[\t ]+TS[0-9]+)?:|ERROR in\b|[^\n]*\[ERROR\]|` +
	`(?:Type|Syntax|Reference|Range)Error:|npm (?:ERR!|error\b)|(?:pnpm )?ERR_PNPM_[A-Z_]+|ELIFECYCLE\b|` +
	`[^\n]*(?:build|transform|compilation) failed\b|Failed to compile\b))`)

var diagnosticSummary = regexp.MustCompile(`^[\t ]*(?:--- FAIL|FAIL\t|panic:|fatal error:|test timed out|[^\s:]+:[0-9]+:[0-9]+:)`)

func Diagnostic(value string, maxBytes int) (string, bool) {
	value = ansi.Strip(strings.ToValidUTF8(value, "�"))
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value, false
	}
	lines := strings.SplitAfter(value, "\n")
	var failures []int
	selected := make([]bool, len(lines))
	timeout, running := false, false
	previouslyDropped := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if count, ok := strings.CutSuffix(strings.TrimPrefix(trimmed, "["), " failure lines dropped]"); ok && strings.HasPrefix(trimmed, "[") {
			if n, err := strconv.Atoi(count); err == nil && n > 0 && n <= math.MaxInt-previouslyDropped-len(lines) {
				previouslyDropped += n
				selected[i] = true
				continue
			}
		}
		if running && (trimmed == "" || (line[0] != '\t' && line[0] != ' ')) {
			running = false
		}
		if timeout && trimmed == "running tests:" {
			running = true
			timeout = false
		}
		if diagnosticSummary.MatchString(line) || diagnosticFailure.MatchString(line) || running {
			if !strings.HasSuffix(line, "\n") {
				lines[i] += "\n"
			}
			failures = append(failures, i)
			selected[i] = true
		}
		if strings.Contains(line, "test timed out") {
			timeout = true
		}
	}
	if len(failures) == 0 {
		return diagnosticContext(value, maxBytes), true
	}
	const marker = "[earlier output truncated]\n"
	notice := ""
	if previouslyDropped > 0 {
		notice = fmt.Sprintf("[%d failure lines dropped]\n", previouslyDropped)
	}
	budget := maxBytes - len(marker) - len(notice)
	total := 0
	for _, i := range failures {
		total += len(lines[i])
	}
	var output strings.Builder
	if total > budget {
		reserve := len(fmt.Sprintf("[%d failure lines dropped]\n", len(failures)+previouslyDropped))
		remaining := maxBytes - reserve
		dropped := previouslyDropped
		for _, i := range failures {
			if len(lines[i]) > remaining {
				dropped++
				continue
			}
			output.WriteString(lines[i])
			remaining -= len(lines[i])
		}
		fmt.Fprintf(&output, "[%d failure lines dropped]\n", dropped)
		return validPrefix(output.String(), maxBytes), true
	}
	for _, i := range failures {
		output.WriteString(lines[i])
	}
	output.WriteString(notice)
	output.WriteString(marker)
	remaining := maxBytes - output.Len()
	contextBudget := remaining / 2
	perFailure := contextBudget / len(failures)
	for _, i := range failures {
		start, end := max(0, i-2), min(len(lines), i+8)
		if i == failures[0] {
			end = len(lines)
		}
		var context strings.Builder
		for j := start; j < end && context.Len() < perFailure; j++ {
			if !selected[j] {
				if len(lines[j]) > perFailure-context.Len() {
					break
				}
				context.WriteString(lines[j])
			}
		}
		output.WriteString(context.String())
	}
	var tail strings.Builder
	for i, line := range lines {
		if !selected[i] {
			tail.WriteString(line)
		}
	}
	output.WriteString(validSuffix(tail.String(), maxBytes-output.Len()))
	return output.String(), true
}

func diagnosticContext(value string, maxBytes int) string {
	const earlier = "[earlier output truncated]\n"
	const omitted = "\n[intervening output truncated]\n"
	if maxBytes <= len(earlier)+len(omitted) {
		return validSuffix(value, maxBytes)
	}
	prefix := earlier
	if failure := diagnosticFailure.FindStringIndex(value); failure != nil {
		start := failure[0]
		for range 2 {
			previous := strings.LastIndexByte(value[:max(0, start-1)], '\n') + 1
			if failure[0]-previous > min(512, maxBytes/8) {
				break
			}
			start = previous
		}
		if start == 0 {
			prefix = ""
		}
		if len(value)-start <= maxBytes-len(prefix) {
			return prefix + validSuffix(value, maxBytes-len(prefix))
		}
		prefixLimit := (maxBytes - len(prefix) - len(omitted)) / 2
		prefix += validPrefix(value[start:], prefixLimit) + omitted
	}
	return prefix + validSuffix(value, maxBytes-len(prefix))
}

func (p Policy) Apply(value string) Text {
	return Truncate(value, p.MaxBytes)
}

func (b *Buffer) Append(value string) {
	if b == nil || value == "" {
		return
	}
	if b.policy.MaxBytes <= 0 {
		b.full.WriteString(value)
		b.originalBytes += len(value)
		return
	}

	nextBytes := b.originalBytes + len(value)
	if !b.truncated && nextBytes <= b.policy.MaxBytes {
		b.full.WriteString(value)
		b.originalBytes = nextBytes
		return
	}

	headMax, tailMax, _ := segmentLimits(b.policy.MaxBytes)
	if !b.truncated {
		current := b.full.String()
		b.head = validPrefix(current, headMax)
		b.headSealed = len(b.head) < len(current) && len(b.head) < headMax
		b.tail = validSuffix(current, tailMax)
		b.full.Reset()
		b.truncated = true
	}

	b.originalBytes = nextBytes
	if missing := headMax - len(b.head); missing > 0 && !b.headSealed {
		prefix := validPrefix(value, missing)
		b.head += prefix
		if len(prefix) < len(value) && len(b.head) < headMax {
			b.headSealed = true
		}
	}
	b.tail = appendSuffix(b.tail, value, tailMax)
}

func (b *Buffer) Text() Text {
	if b == nil {
		return Text{}
	}
	if !b.truncated {
		return Text{Value: b.full.String()}
	}

	_, _, marker := segmentLimits(b.policy.MaxBytes)
	strategy := StrategyHeadTail
	value := b.head + marker + b.tail
	if marker == "" {
		strategy = StrategyPrefix
		value = validPrefix(b.head, b.policy.MaxBytes)
	}
	storedBytes := len(value)
	omittedBytes := b.originalBytes - storedBytes
	if omittedBytes < 0 {
		omittedBytes = 0
	}

	return Text{
		Value: value,
		Truncation: &Truncation{
			Truncated:     true,
			OriginalBytes: b.originalBytes,
			StoredBytes:   storedBytes,
			LimitBytes:    b.policy.MaxBytes,
			OmittedBytes:  omittedBytes,
			Strategy:      strategy,
		},
	}
}

func (b *Buffer) String() string {
	return b.Text().Value
}

func (b *Buffer) Truncation() *Truncation {
	text := b.Text()
	if text.Truncation == nil {
		return nil
	}
	return CloneTruncation(text.Truncation)
}

func CloneTruncation(meta *Truncation) *Truncation {
	if meta == nil {
		return nil
	}
	clone := *meta
	return &clone
}

func segmentLimits(maxBytes int) (int, int, string) {
	if maxBytes <= 0 {
		return 0, 0, ""
	}
	if maxBytes <= len(Marker)+2 {
		return maxBytes, 0, ""
	}
	contextBytes := maxBytes - len(Marker)
	headBytes := contextBytes / 2
	tailBytes := contextBytes - headBytes
	return headBytes, tailBytes, Marker
}

func appendSuffix(current string, value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) >= maxBytes {
		return validSuffix(value, maxBytes)
	}
	return validSuffix(current+value, maxBytes)
}

func validPrefix(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	end := 0
	for end < len(value) {
		_, size := utf8.DecodeRuneInString(value[end:])
		if size <= 0 || end+size > maxBytes {
			break
		}
		end += size
	}
	return value[:end]
}

func validSuffix(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	start := len(value)
	for start > 0 {
		_, size := utf8.DecodeLastRuneInString(value[:start])
		if size <= 0 {
			break
		}
		next := start - size
		if len(value)-next > maxBytes {
			break
		}
		start = next
	}
	return value[start:]
}
