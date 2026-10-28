package main

import (
	"io"
	"testing"
)

func TestRunRejectsInvalidInvocation(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{"missing pin", nil},
		{"zero duration", []string{"-pin", "example", "-duration", "0s"}},
		{"unexpected argument", []string{"-pin", "example", "extra"}},
		{"unbounded sweep samples", []string{"-pin", "example", "-sweep-passes", "1001"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := run(t.Context(), test.args, io.Discard); err == nil {
				t.Fatal("invalid invocation started a soak")
			}
		})
	}
	t.Run("missing isolated scratch", func(t *testing.T) {
		for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
			t.Setenv(key, "")
		}
		if err := run(t.Context(), []string{"-pin", "example"}, io.Discard); err == nil {
			t.Fatal("started without isolated scratch")
		}
	})
}
