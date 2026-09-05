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
		"visual-review/viewer.js": "81b084b8abbe21d986512e72ef41652a31b305a2ce41265bf919abed84a049b8",
		"visual-review/schema.js": "d856206373b444e54b8d512648c5c7b675b376e51147a8a809f555ca4d445fe5",
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
