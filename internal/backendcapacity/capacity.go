package backendcapacity

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	ErrorClass                  = "backend_capacity"
	TransientOverloadErrorClass = "transient_overload"
	StartupFailureErrorClass    = "backend_startup_failure"
	StartupTimeoutErrorClass    = "backend_startup_timeout"
	StartupFailureKind          = "startup_handshake_failure"
	StartupTimeoutKind          = "startup_handshake_timeout"
)

type ErrorType string

const (
	ErrorTypeUsageLimit        ErrorType = "usage_limit"
	ErrorTypeTransientOverload ErrorType = "transient_overload"
)

var retryAtPattern = regexp.MustCompile(`(?i)(?:try again|resets?)\s+(?:at\s+)?([0-9]{1,2}:[0-9]{2}\s*(?:am|pm))(?:\s*\(([A-Za-z0-9._+-]+(?:/[A-Za-z0-9._+-]+)*)\))?`)
var retryDatePattern = regexp.MustCompile(`(?i)(?:try again|resets?)\s+(?:at\s+)?([A-Za-z]{3,9})\s+([0-9]{1,2})(?:st|nd|rd|th)?,?\s+([0-9]{4})\s+([0-9]{1,2}:[0-9]{2}\s*(?:am|pm))(?:\s*\(([A-Za-z0-9._+-]+(?:/[A-Za-z0-9._+-]+)*)\))?`)
var http5xxPattern = regexp.MustCompile(`(?i)(?:http(?:/[0-9.]+)?\s+|status(?:[\s_-]*code)?["']?\s*[:=]?\s*)(5[0-9]{2})\b`)

type Scope struct {
	BackendID   string
	BackendKind string
	Provider    string
}

func (s Scope) Normalize() Scope {
	return Scope{
		BackendID:   strings.TrimSpace(s.BackendID),
		BackendKind: strings.TrimSpace(s.BackendKind),
		Provider:    strings.TrimSpace(s.Provider),
	}
}

func (s Scope) Key() string {
	s = s.Normalize()
	return strings.ToLower(s.BackendID + "\x00" + s.BackendKind + "\x00" + s.Provider)
}

func (s Scope) Matches(other Scope) bool {
	s = s.Normalize()
	other = other.Normalize()
	if !strings.EqualFold(s.BackendID, other.BackendID) || !strings.EqualFold(s.BackendKind, other.BackendKind) {
		return false
	}
	return s.Provider == "" || other.Provider == "" || strings.EqualFold(s.Provider, other.Provider)
}

func (s Scope) Hosted() bool {
	provider := normalizeToken(s.Provider)
	return provider == "" || (!strings.Contains(provider, "ollama") && !strings.HasPrefix(provider, "local"))
}

type Details struct {
	Type    ErrorType
	Kind    string
	Reason  string
	ResetAt *time.Time
}

type Error struct {
	Scope   Scope
	Details Details
	Err     error
}

func NewError(scope Scope, details Details, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Scope: scope.Normalize(), Details: cloneDetails(details), Err: err}
}

func (e *Error) Error() string {
	if e == nil || e.Err == nil {
		return "backend capacity exhausted"
	}
	return e.Err.Error()
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func As(err error) (*Error, bool) {
	var capacityErr *Error
	if !errors.As(err, &capacityErr) || capacityErr == nil {
		return nil, false
	}
	cloned := *capacityErr
	cloned.Scope = cloned.Scope.Normalize()
	cloned.Details = cloneDetails(cloned.Details)
	return &cloned, true
}

type Rules struct {
	Kinds        []string
	Phrases      []string
	HTTP5xx      bool
	RequireReset bool
}

func Classify(text string, fallbackResetAt *time.Time, now time.Time, rules Rules) (Details, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Details{}, false
	}
	kind, ok := matchingKind(text, rules)
	if !ok {
		return Details{}, false
	}
	resetAt := resetFromText(text, now)
	if resetAt == nil && fallbackResetAt != nil {
		value := fallbackResetAt.UTC()
		resetAt = &value
	}
	if rules.RequireReset && resetAt == nil {
		return Details{}, false
	}
	return Details{
		Type:    ErrorTypeUsageLimit,
		Kind:    kind,
		Reason:  "provider usage limit reached",
		ResetAt: resetAt,
	}, true
}

func ClassifyTransientOverload(text string, rules Rules) (Details, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Details{}, false
	}
	kind, ok := matchingKind(text, rules)
	if !ok && rules.HTTP5xx {
		match := http5xxPattern.FindStringSubmatch(text)
		if len(match) == 2 {
			kind = "http_" + match[1]
			ok = true
		}
	}
	if !ok {
		return Details{}, false
	}
	return Details{
		Type:   ErrorTypeTransientOverload,
		Kind:   kind,
		Reason: string(ErrorTypeTransientOverload),
	}, true
}

func IsTransientOverload(err error) bool {
	capacityErr, ok := As(err)
	return ok && capacityErr != nil && capacityErr.Details.Type == ErrorTypeTransientOverload
}

func IsStartupFailureKind(kind string) bool {
	kind = strings.TrimSpace(kind)
	return kind == StartupFailureKind || kind == StartupTimeoutKind
}

func cloneDetails(details Details) Details {
	if details.ResetAt != nil {
		resetAt := *details.ResetAt
		details.ResetAt = &resetAt
	}
	return details
}

func matchingKind(text string, rules Rules) (string, bool) {
	normalizedText := normalizeToken(text)
	for _, kind := range rules.Kinds {
		if normalized := normalizeToken(kind); normalized != "" && strings.Contains(normalizedText, normalized) {
			return strings.TrimSpace(kind), true
		}
	}
	lowerText := strings.ToLower(text)
	for _, phrase := range rules.Phrases {
		if phrase = strings.TrimSpace(phrase); phrase != "" && strings.Contains(lowerText, strings.ToLower(phrase)) {
			return normalizeToken(phrase), true
		}
	}
	return "", false
}

func normalizeToken(value string) string {
	var b strings.Builder
	for _, char := range strings.ToLower(strings.TrimSpace(value)) {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			b.WriteRune(char)
		}
	}
	return b.String()
}

func resetFromText(text string, now time.Time) *time.Time {
	if resetAt := resetFromJSON([]byte(text), now); resetAt != nil {
		return resetAt
	}
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start >= 0 && end > start {
		if resetAt := resetFromJSON([]byte(text[start:end+1]), now); resetAt != nil {
			return resetAt
		}
	}
	if resetAt := resetFromDatedText(text, now); resetAt != nil {
		return resetAt
	}
	match := retryAtPattern.FindStringSubmatch(text)
	if len(match) != 3 {
		return nil
	}
	location := now.Location()
	if timezone := strings.TrimSpace(match[2]); timezone != "" {
		var err error
		location, err = time.LoadLocation(timezone)
		if err != nil {
			return nil
		}
	}
	now = now.In(location)
	clock := strings.ToUpper(strings.Join(strings.Fields(match[1]), ""))
	parsed, err := time.ParseInLocation("3:04PM", clock, location)
	if err != nil {
		return nil
	}
	resetAt := time.Date(now.Year(), now.Month(), now.Day(), parsed.Hour(), parsed.Minute(), 0, 0, location)
	if !resetAt.After(now) {
		resetAt = resetAt.AddDate(0, 0, 1)
	}
	resetAt = resetAt.UTC()
	return &resetAt
}

func resetFromDatedText(text string, now time.Time) *time.Time {
	match := retryDatePattern.FindStringSubmatch(text)
	if len(match) != 6 {
		return nil
	}
	location := now.Location()
	if timezone := strings.TrimSpace(match[5]); timezone != "" {
		var err error
		location, err = time.LoadLocation(timezone)
		if err != nil {
			return nil
		}
	}
	month := strings.ToLower(strings.TrimSpace(match[1]))
	if month == "" {
		return nil
	}
	month = strings.ToUpper(month[:1]) + month[1:]
	value := strings.Join([]string{
		month,
		strings.TrimSpace(match[2]) + ",",
		strings.TrimSpace(match[3]),
		strings.ToUpper(strings.Join(strings.Fields(match[4]), "")),
	}, " ")
	resetAt, err := time.ParseInLocation("Jan 2, 2006 3:04PM", value, location)
	if err != nil {
		return nil
	}
	resetAt = resetAt.UTC()
	return &resetAt
}

func resetFromJSON(data []byte, now time.Time) *time.Time {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || (data[0] != '{' && data[0] != '[') {
		return nil
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil
	}
	return resetFromValue(value, "", now)
}

func resetFromValue(value any, key string, now time.Time) *time.Time {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for childKey := range typed {
			keys = append(keys, childKey)
		}
		slices.SortFunc(keys, func(left string, right string) int {
			leftPriority := resetKeyPriority(left)
			rightPriority := resetKeyPriority(right)
			if leftPriority != rightPriority {
				return leftPriority - rightPriority
			}
			return strings.Compare(left, right)
		})
		for _, childKey := range keys {
			childValue := typed[childKey]
			if resetAt := resetFromValue(childValue, childKey, now); resetAt != nil {
				return resetAt
			}
		}
	case []any:
		for _, childValue := range typed {
			if resetAt := resetFromValue(childValue, key, now); resetAt != nil {
				return resetAt
			}
		}
	case json.Number:
		return resetFromNumber(string(typed), key, now)
	case float64:
		return resetFromNumber(strconv.FormatFloat(typed, 'f', -1, 64), key, now)
	case string:
		return resetFromString(typed, key, now)
	}
	return nil
}

func resetKeyPriority(key string) int {
	switch normalizeToken(key) {
	case "resetat", "resetsat", "resettime", "resettimestamp":
		return 0
	case "retryafter", "retryafterseconds", "resetin", "resetinseconds", "resetsin", "resetsinseconds":
		return 1
	default:
		return 2
	}
}

func resetFromNumber(value string, key string, now time.Time) *time.Time {
	normalizedKey := normalizeToken(key)
	number, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return nil
	}
	switch normalizedKey {
	case "retryafter", "retryafterseconds", "resetin", "resetinseconds", "resetsin", "resetsinseconds":
		resetAt := now.Add(time.Duration(number * float64(time.Second))).UTC()
		return &resetAt
	case "resetat", "resetsat", "resettime", "resettimestamp":
		if number > 1_000_000_000_000 {
			number /= 1000
		}
		resetAt := time.Unix(int64(number), 0).UTC()
		return &resetAt
	default:
		return nil
	}
}

func resetFromString(value string, key string, now time.Time) *time.Time {
	normalizedKey := normalizeToken(key)
	switch normalizedKey {
	case "resetat", "resetsat", "resettime", "resettimestamp":
		if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value)); err == nil {
			parsed = parsed.UTC()
			return &parsed
		}
		return resetFromNumber(value, key, now)
	case "retryafter", "retryafterseconds", "resetin", "resetinseconds", "resetsin", "resetsinseconds":
		return resetFromNumber(value, key, now)
	default:
		return nil
	}
}
