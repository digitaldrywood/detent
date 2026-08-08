package detent

import (
	"bytes"
	"io/fs"
	"testing"

	"github.com/digitaldrywood/detent/internal/operatorskill"
)

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
