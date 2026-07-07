package runtimeoutput

import (
	"strings"
	"unicode/utf8"
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
		b.tail = validSuffix(current, tailMax)
		b.full.Reset()
		b.truncated = true
	}

	b.originalBytes = nextBytes
	if missing := headMax - len(b.head); missing > 0 {
		b.head += validPrefix(value, missing)
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
