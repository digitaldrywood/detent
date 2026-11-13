package hubserver

import (
	"strings"
)

func validationRepository(repository string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(repository, "https://github.com/"), "git@github.com:"), ".git")
}
