package runner

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
)

func TestWorkspaceEvidenceSource(t *testing.T) {
	t.Parallel()
	root, outside := t.TempDir(), t.TempDir()
	logContent := []byte("browser verification passed\n")
	var pngData, jpegData, gifData bytes.Buffer
	picture := image.NewRGBA(image.Rect(0, 0, 2, 3))
	if err := png.Encode(&pngData, picture); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&jpegData, picture, nil); err != nil {
		t.Fatal(err)
	}
	if err := gif.Encode(&gifData, picture, nil); err != nil {
		t.Fatal(err)
	}
	webp, err := base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"verification.log": logContent,
		"verification.LOG": logContent,
		"note.txt":         []byte("plain text\n"),
		"note.md":          []byte("# Verification\n"),
		"results.csv":      []byte("test,result\nbrowser,passed\n"),
		"results.json":     []byte(`{"browser":"passed"}`),
		"screenshot.png":   pngData.Bytes(),
		"screenshot.jpeg":  jpegData.Bytes(),
		"screenshot.gif":   gifData.Bytes(),
		"screenshot.webp":  webp,
		"untyped":          logContent,
		"page.html":        []byte("<html>page</html>"),
		"page.log":         []byte("<html>page</html>"),
		"padded.log":       append(bytes.Repeat([]byte(" "), 600), []byte("<html>page</html>")...),
		"bom.log":          []byte("\xef\xbb\xbf<html>page</html>"),
		"svg.log":          []byte("<svg xmlns=\"http://www.w3.org/2000/svg\"/>"),
		"xml.log":          []byte("<?xml version=\"1.0\"?><page/>"),
		"binary.log":       {0, 1, 2},
		"invalid-utf8.log": {0xff, 0xfe},
		"invalid.json":     []byte("{invalid}"),
		"mismatch.png":     logContent,
		"document.pdf":     []byte("%PDF-1.7\nfile"),
		"empty.log":        {},
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.log"), logContent, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(root, "large.log"))
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(file.Truncate(attachment.MaxBytes+1), file.Close()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("verification.log", filepath.Join(root, "inside.log")); err != nil {
		t.Fatal(err)
	}
	files["inside.log"] = logContent
	source := workspaceEvidenceSource(root)
	for _, test := range []struct {
		name     string
		path     string
		media    string
		allowed  bool
		tooLarge bool
	}{
		{name: "log", path: "verification.log", media: "text/plain", allowed: true},
		{name: "uppercase log", path: "verification.LOG", media: "text/plain", allowed: true},
		{name: "text", path: "note.txt", media: "text/plain", allowed: true},
		{name: "markdown", path: "note.md", media: "text/markdown", allowed: true},
		{name: "csv", path: "results.csv", media: "text/csv", allowed: true},
		{name: "json", path: "results.json", media: "application/json", allowed: true},
		{name: "png", path: "screenshot.png", media: "image/png", allowed: true},
		{name: "jpeg", path: "screenshot.jpeg", media: "image/jpeg", allowed: true},
		{name: "gif", path: "screenshot.gif", media: "image/gif", allowed: true},
		{name: "webp", path: "screenshot.webp", media: "image/webp", allowed: true},
		{name: "inferred text", path: "untyped", media: "text/plain", allowed: true},
		{name: "symlink within workspace", path: "inside.log", media: "text/plain", allowed: true},
		{name: "absolute", path: filepath.Join(outside, "secret.log")},
		{name: "parent traversal", path: "../secret.log"},
		{name: "symlink escape", path: "escape/secret.log"},
		{name: "oversize", path: "large.log", tooLarge: true},
		{name: "directory", path: "."},
		{name: "active content", path: "page.html"},
		{name: "html as log", path: "page.log"},
		{name: "html beyond sniff window", path: "padded.log"},
		{name: "html with BOM", path: "bom.log"},
		{name: "svg as log", path: "svg.log"},
		{name: "xml as log", path: "xml.log"},
		{name: "binary as log", path: "binary.log"},
		{name: "invalid UTF-8 as log", path: "invalid-utf8.log"},
		{name: "invalid json", path: "invalid.json"},
		{name: "image mismatch", path: "mismatch.png"},
		{name: "unsupported pdf", path: "document.pdf"},
		{name: "empty", path: "empty.log"},
		{name: "missing", path: "missing.log"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := source(t.Context(), test.path)
			if (err == nil) != test.allowed || test.tooLarge && !errors.Is(err, attachment.ErrTooLarge) {
				t.Fatalf("read %q: %+v, %v", test.path, result, err)
			}
			if test.allowed && (result.Name != filepath.Base(test.path) || result.ContentType != test.media || !bytes.Equal(result.Content, files[test.path])) {
				t.Fatalf("workspace evidence = %+v", result)
			}
		})
	}
	remote := SSHExecutionSources{evidence: source, directory: outside}
	path, err := json.Marshal("verification.log")
	if err != nil {
		t.Fatal(err)
	}
	result, err := remote.Handle(t.Context(), "source.evidence", []json.RawMessage{path})
	if err != nil || string(result.(ValidationEvidence).Content) != "browser verification passed\n" {
		t.Fatalf("remote workspace evidence = %+v, %v", result, err)
	}
}
