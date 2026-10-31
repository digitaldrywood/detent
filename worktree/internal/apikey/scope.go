package apikey

import (
	"errors"
	"strings"
)

type Scope string

const (
	ScopeRead  Scope = "read"
	ScopeWrite Scope = "write"
	ScopeAdmin Scope = "admin"
)

var ErrInvalidScope = errors.New("invalid scope")

func NormalizeScopes(values []string) ([]string, error) {
	seen := map[string]bool{}
	scopes := make([]string, 0, len(values))
	for _, value := range values {
		scope := Scope(strings.ToLower(strings.TrimSpace(value)))
		if scope == "" {
			continue
		}
		if !ValidScope(scope) {
			return nil, ErrInvalidScope
		}
		text := string(scope)
		if seen[text] {
			continue
		}
		seen[text] = true
		scopes = append(scopes, text)
	}
	if len(scopes) == 0 {
		scopes = append(scopes, string(ScopeWrite))
	}
	return scopes, nil
}

func NormalizeProjectIDs(values []string) []string {
	seen := map[string]bool{}
	projectIDs := make([]string, 0, len(values))
	for _, value := range values {
		projectID := strings.TrimSpace(value)
		if projectID == "" || seen[projectID] {
			continue
		}
		seen[projectID] = true
		projectIDs = append(projectIDs, projectID)
	}
	return projectIDs
}

func ValidScope(scope Scope) bool {
	switch scope {
	case ScopeRead, ScopeWrite, ScopeAdmin:
		return true
	default:
		return false
	}
}

func HasScope(granted []string, required Scope) bool {
	if !ValidScope(required) {
		return false
	}
	requiredRank := scopeRank(required)
	for _, value := range granted {
		scope := Scope(strings.ToLower(strings.TrimSpace(value)))
		if !ValidScope(scope) {
			continue
		}
		if scopeRank(scope) >= requiredRank {
			return true
		}
	}
	return false
}

func HighestScope(scopes []string) Scope {
	highest := ScopeRead
	for _, value := range scopes {
		scope := Scope(strings.ToLower(strings.TrimSpace(value)))
		if ValidScope(scope) && scopeRank(scope) > scopeRank(highest) {
			highest = scope
		}
	}
	return highest
}

func AllowsProject(projectIDs []string, projectID string) bool {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" || len(projectIDs) == 0 {
		return true
	}
	for _, allowed := range projectIDs {
		if strings.TrimSpace(allowed) == projectID {
			return true
		}
	}
	return false
}

func scopeRank(scope Scope) int {
	switch scope {
	case ScopeAdmin:
		return 3
	case ScopeWrite:
		return 2
	case ScopeRead:
		return 1
	default:
		return 0
	}
}
