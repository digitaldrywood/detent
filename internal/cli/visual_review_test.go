package cli

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestRunVisualReviewImportRejectsInvalidPackageBeforeHTTPRequest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	opts := defaultOptions()
	opts.httpDo = func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("unexpected HTTP request")
	}
	if _, err := runVisualReviewImport(context.Background(), "", "", -1, false, "detent", "issue", root, opts); err == nil {
		t.Fatal("invalid package accepted")
	}
	if calls != 0 {
		t.Fatalf("HTTP calls = %d, want 0", calls)
	}
}

func TestSafeVisualReviewPackageAssetRejectsSymlinkEscape(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.png")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "image.png")); err != nil {
		t.Fatal(err)
	}
	if _, err := safeVisualReviewPackageAsset(root, "image.png"); err == nil {
		t.Fatal("escaping symlink accepted")
	}
}

func TestSafeVisualReviewPackageAssetAcceptsContainedRegularPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "media", "image.png")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := safeVisualReviewPackageAsset(root, "media/image.png")
	want, resolveErr := filepath.EvalSymlinks(path)
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	if err != nil || got != want {
		t.Fatalf("safe path = %q, %v", got, err)
	}
}
