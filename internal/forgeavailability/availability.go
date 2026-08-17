package forgeavailability

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

const (
	Condition      = "forge_unavailable"
	ClassServer    = "server"
	ClassTimeout   = "timeout"
	ClassTransport = "transport"
)

var (
	ErrUnavailable = errors.New("forge unavailable")
	httpStatusRE   = regexp.MustCompile(`(?i)(?:http(?:/\d(?:\.\d)?)?|status)[^0-9]{0,16}([1-5][0-9]{2})`)
	urlHostRE      = regexp.MustCompile(`(?i)\b(?:https?|ssh|git)://([^/@\s]+@)?([^/:\s]+)`)
	scpHostRE      = regexp.MustCompile(`(?i)\b(?:[^@\s]+@)([a-z0-9][a-z0-9.-]*):`)
)

type Scope struct {
	Host      string `json:"host"`
	Operation string `json:"operation"`
}

func (s Scope) Normalize() Scope {
	s.Host = NormalizeHost(s.Host)
	s.Operation = strings.ToLower(strings.TrimSpace(s.Operation))
	return s
}

func (s Scope) Key() string {
	return s.Normalize().Host
}

type Error struct {
	Scope Scope
	Class string
	Err   error
}

func NewError(scope Scope, class string, err error) *Error {
	return &Error{
		Scope: scope.Normalize(),
		Class: strings.ToLower(strings.TrimSpace(class)),
		Err:   err,
	}
}

func (e *Error) Error() string {
	if e == nil {
		return ErrUnavailable.Error()
	}
	host := e.Scope.Host
	if host == "" {
		host = "configured"
	}
	message := "forge " + host + " unavailable (" + Condition
	if e.Class != "" {
		message += "/" + e.Class
	}
	message += ")"
	if e.Err != nil {
		message += ": " + e.Err.Error()
	}
	return message
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *Error) Is(target error) bool {
	return target == ErrUnavailable
}

func As(err error) (*Error, bool) {
	var availabilityErr *Error
	if !errors.As(err, &availabilityErr) || availabilityErr == nil {
		return nil, false
	}
	return availabilityErr, true
}

func Classify(operation string, detail string) (string, bool) {
	operation = strings.ToLower(strings.TrimSpace(operation))
	detail = strings.ToLower(strings.TrimSpace(detail))
	if !WriteOperation(operation) || detail == "" {
		return "", false
	}
	for _, excluded := range []string{
		"http 401", "http 403", "http 429", "status 401", "status 403", "status 429",
		"non-fast-forward", "protected branch", "pre-receive hook declined", "hook declined",
		"remote rejected", "[rejected]", "permission denied (publickey)", "authentication failed",
		"repository not found",
	} {
		if strings.Contains(detail, excluded) {
			return "", false
		}
	}
	for _, match := range httpStatusRE.FindAllStringSubmatch(detail, -1) {
		if len(match) == 2 && strings.HasPrefix(match[1], "5") {
			return ClassServer, true
		}
		if len(match) == 2 && (match[1] == "401" || match[1] == "403" || match[1] == "429") {
			return "", false
		}
	}
	for _, timeout := range []string{
		"context deadline exceeded", "operation timed out", "operation timeout", "i/o timeout",
		"connection timed out", "connection timeout", "tls handshake timeout", "timeout awaiting response",
	} {
		if strings.Contains(detail, timeout) {
			return ClassTimeout, true
		}
	}
	for _, transport := range []string{
		"could not resolve host", "temporary failure in name resolution", "name or service not known",
		"no such host", "network is unreachable", "connection refused", "connection reset by peer",
		"failed to connect", "tls handshake", "ssl connect error", "certificate verify failed",
		"remote end hung up unexpectedly", "unexpected disconnect", "early eof",
		"kex_exchange_identification: connection closed", "connection closed by remote host",
	} {
		if strings.Contains(detail, transport) {
			return ClassTransport, true
		}
	}
	return "", false
}

func ProvesReachability(operation string, detail string) bool {
	if !WriteOperation(operation) {
		return false
	}
	detail = strings.ToLower(strings.TrimSpace(detail))
	for _, match := range httpStatusRE.FindAllStringSubmatch(detail, -1) {
		if len(match) == 2 && !strings.HasPrefix(match[1], "5") {
			return true
		}
	}
	for _, evidence := range []string{
		"non-fast-forward", "protected branch", "pre-receive hook declined", "hook declined",
		"remote rejected", "[rejected]", "permission denied (publickey)", "authentication failed",
		"repository not found",
	} {
		if strings.Contains(detail, evidence) {
			return true
		}
	}
	return false
}

func WriteOperation(operation string) bool {
	operation = strings.ToLower(strings.TrimSpace(operation))
	if strings.Contains(operation, "ci-trigger-label") {
		return false
	}
	if strings.Contains(operation, "git push") || strings.Contains(operation, "git fetch") || strings.HasPrefix(operation, "push") {
		return true
	}
	if strings.Contains(operation, "create_pull_request") || strings.Contains(operation, "update_pull_request") || strings.Contains(operation, "edit_pull_request") {
		return true
	}
	for _, command := range []string{"gh pr create", "gh pr edit", "gh pr ready"} {
		if strings.Contains(operation, command) {
			return true
		}
	}
	return strings.Contains(operation, "gh api") && strings.Contains(operation, "/pulls")
}

func HostFromText(value string) string {
	if match := urlHostRE.FindStringSubmatch(value); len(match) == 3 {
		return NormalizeHost(match[2])
	}
	if match := scpHostRE.FindStringSubmatch(value); len(match) == 2 {
		return NormalizeHost(match[1])
	}
	return ""
}

func HostFromEndpoint(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return ""
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() == "" {
		return NormalizeHost(endpoint)
	}
	host := NormalizeHost(parsed.Hostname())
	if host == "api.github.com" {
		return "github.com"
	}
	return host
}

func NormalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimSuffix(host, ".")
	if strings.Contains(host, "://") {
		return HostFromEndpoint(host)
	}
	if parsed, err := url.Parse("//" + host); err == nil && parsed.Hostname() != "" {
		host = strings.ToLower(parsed.Hostname())
	}
	return host
}
