package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/operatortool/capability"
)

func TestGenerateModeAndOutput(t *testing.T) {
	t.Parallel()
	for _, sources := range []bool{false, true} {
		name := "document"
		if sources {
			name = "sources"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "docs"), 0755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "docs/mcp-capability-matrix.md")
			original := []byte("existing document")
			if err := os.WriteFile(path, original, 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(root, "internal/web"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "internal/web/server.go"), []byte(`package web; import "github.com/labstack/echo/v4"; func routes(router *echo.Echo) { router.GET("/fixture", handler) }`), 0644); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if err := generate(root, sources, &output); err != nil {
				t.Fatal(err)
			}
			document, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if sources {
				var sites []capability.Candidate
				if err := json.Unmarshal(output.Bytes(), &sites); err != nil || len(sites) != 1 {
					t.Fatalf("source output = %s, %v", output.Bytes(), err)
				}
				if !bytes.Equal(document, original) {
					t.Fatal("source listing overwrote the document")
				}
			} else if output.Len() != 0 || bytes.Equal(document, original) || len(document) == 0 {
				t.Fatal("document generation did not replace the file silently")
			}
		})
	}
}

func TestGenerateReportsIOFailures(t *testing.T) {
	t.Parallel()
	writeError := errors.New("output unavailable")
	for _, test := range []struct {
		name    string
		sources bool
		missing bool
		writer  bool
	}{
		{"missing document directory", false, false, false},
		{"missing source root", true, true, false},
		{"source output failure", true, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if test.missing {
				root = filepath.Join(root, "missing")
			}
			var output bytes.Buffer
			var err error
			if test.writer {
				err = generate(root, test.sources, failingWriter{writeError})
			} else {
				err = generate(root, test.sources, &output)
			}
			if err == nil || test.writer && !errors.Is(err, writeError) {
				t.Fatalf("generate() = %v", err)
			}
		})
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) {
	return 0, w.err
}
