package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSampleConfigsLoadAndValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
	}{
		{name: "minimal", path: "config.example.yaml"},
		{name: "annotated", path: "config.annotated.yaml"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join("..", "..", test.path)
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(%q) error = %v", path, err)
			}
			document := make([]byte, 0, len(content)+32)
			document = append(document, "---\n"...)
			document = append(document, content...)
			document = append(document, "\n---\nSample workflow\n"...)
			workflow, err := ParseWorkflow(document)
			if err != nil {
				t.Fatalf("ParseWorkflow() error = %v", err)
			}
			if err := workflow.Config.Validate(); err != nil {
				t.Fatalf("Config.Validate() error = %v", err)
			}
		})
	}
}
