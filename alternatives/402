package detent

import (
	"bytes"
	"testing"

	"github.com/digitaldrywood/detent/internal/operatorskill"
)

func TestOperatorSkillContentMatchesBundle(t *testing.T) {
	if content := OperatorSkillContent(); !bytes.Equal(content, operatorskill.Content()) {
		t.Fatal("OperatorSkillContent() does not match the source bundle")
	}
}
