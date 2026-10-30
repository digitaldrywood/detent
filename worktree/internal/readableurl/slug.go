package readableurl

import (
	"errors"
	"slices"
	"strings"
)

var Reserved = []string{"work", "settings", "login", "projects", "organizations", "organization", "api", "app", "auth", "chat", "reports", "activity", "diagnostics", "fleet", "support", "threads", "invite", "logout", "health", "metrics", "static", "webhooks", "design-system", "setup", "usage", "billing", "account", "platform"}

func Validate(slug string) error {
	if len(slug) == 0 || len(slug) > 80 || slug[0] == '-' || slug[len(slug)-1] == '-' || strings.Contains(slug, "--") || slices.Contains(Reserved, slug) || strings.IndexFunc(slug, func(r rune) bool { return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' }) != -1 {
		return errors.New("use 1–80 lowercase letters, numbers and single hyphens, and an unreserved name")
	}
	return nil
}
