package testenv

import (
	"os"
	"strings"
	"testing"
)

func TestClearGitEnvironment(t *testing.T) {
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") {
			t.Setenv(key, value)
		}
	}

	tests := []struct {
		key     string
		removed bool
	}{
		{key: "GIT_DIR", removed: true},
		{key: "GIT_COMMON_DIR", removed: true},
		{key: "GIT_WORK_TREE", removed: true},
		{key: "GIT_INDEX_FILE", removed: true},
		{key: "GIT_OBJECT_DIRECTORY", removed: true},
		{key: "GIT_ALTERNATE_OBJECT_DIRECTORIES", removed: true},
		{key: "GIT_CONFIG", removed: true},
		{key: "GIT_CONFIG_PARAMETERS", removed: true},
		{key: "GIT_CONFIG_COUNT", removed: true},
		{key: "GIT_CONFIG_KEY_0", removed: true},
		{key: "GIT_CONFIG_VALUE_0", removed: true},
		{key: "GIT_AUTHOR_NAME", removed: true},
		{key: "GIT_COMMITTER_EMAIL", removed: true},
		{key: "DETENT_TEST_KEEP_ENV"},
	}
	for _, tt := range tests {
		t.Setenv(tt.key, "inherited value")
	}
	if err := ClearGitEnvironment(); err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			value, present := os.LookupEnv(tt.key)
			if present == tt.removed {
				t.Fatalf("environment variable present = %t, want %t", present, !tt.removed)
			}
			if !tt.removed && value != "inherited value" {
				t.Fatalf("preserved value = %q, want inherited value", value)
			}
		})
	}
}
