package cli_test

import (
	"bytes"
	"testing"

	"github.com/digitaldrywood/detent"
	"github.com/digitaldrywood/detent/internal/operatorskill"
)

func TestOperatorSkillContentMatchesBundle(t *testing.T) {
	if content := detent.OperatorSkillContent(); !bytes.Equal(content, operatorskill.Content()) {
		t.Fatal("OperatorSkillContent() does not match the source bundle")
	}
}
