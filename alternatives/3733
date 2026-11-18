package projectstore

import (
	"fmt"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/selector"
)

func TestModelPartsRejectOversizedPage(t *testing.T) {
	for _, kind := range []string{"levels", "selector tree"} {
		t.Run(kind, func(t *testing.T) {
			m := config.ModelSelection{}
			if kind == "levels" {
				m.Levels = map[string]config.ModelSelectionDefaults{}
				for i := 0; i < PagePartLimit; i++ {
					m.Levels[fmt.Sprint(i)] = config.ModelSelectionDefaults{}
				}
			} else {
				children := make([]selector.Selector, PagePartLimit)
				m.Rules = &[]config.ModelSelectionRule{{Selector: selector.Selector{Or: children}}}
			}
			if _, err := ModelParts(m); err == nil {
				t.Fatal("unbounded model page accepted")
			}
		})
	}
}
