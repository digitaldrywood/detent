package detent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"testing"

	"github.com/digitaldrywood/detent/internal/operatorskill"
)

func TestVendoredVisualReviewAssetsMatchPinnedUpstream(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"visual-review/viewer.js": "126adf8df010f7d802903f0bd076b4e0f42c9e5f7f5611b810bca45db7e9a16d",
		"visual-review/schema.js": "9308f24ffcffeb8e995a95743b8a9d995151868901b0df3b971a12bf94028bb0",
		"visual-review/style.css": "4564ec5ffc99bf0b0bdfdee6bdf5b7c539eeb929c4e77ac02eda6f64c00ebe27",
	}
	for path, expected := range want {
		data, err := fs.ReadFile(StaticFS(), path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != expected {
			t.Errorf("%s digest = %q (%d), want %q (%d)", path, got, len(got), expected, len(expected))
		}
	}
}

func TestStaticFSContainsCSSOutput(t *testing.T) {
	data, err := fs.ReadFile(StaticFS(), "css/output.css")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if len(data) == 0 {
		t.Fatal("css/output.css is empty")
	}
	if !bytes.Contains(data, []byte("tailwindcss")) {
		t.Fatalf("css/output.css missing Tailwind marker:\n%s", data)
	}
}

func TestOperatorSkillContentMatchesBundle(t *testing.T) {
	if content := OperatorSkillContent(); !bytes.Equal(content, operatorskill.Content()) {
		t.Fatal("OperatorSkillContent() does not match the source bundle")
	}
}
