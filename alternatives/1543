package hubserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/chat"
)

func TestValidateCoordinatorIssueSplit(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*chat.IssueSplit)
		valid  bool
	}{
		{"independent children", func(*chat.IssueSplit) {}, true},
		{"child chain and parent", func(s *chat.IssueSplit) {
			s.Edges = []chat.IssueSplitEdge{{Dependent: 2, Blocker: 1}, {Dependent: 0, Blocker: 2}}
		}, true},
		{"cycle", func(s *chat.IssueSplit) {
			s.Edges = []chat.IssueSplitEdge{{Dependent: 2, Blocker: 1}, {Dependent: 1, Blocker: 2}}
		}, false},
		{"parent cycle", func(s *chat.IssueSplit) {
			s.Edges = []chat.IssueSplitEdge{{Dependent: 0, Blocker: 1}, {Dependent: 1, Blocker: 0}}
		}, false},
		{"self edge", func(s *chat.IssueSplit) { s.Edges = []chat.IssueSplitEdge{{Dependent: 1, Blocker: 1}} }, false},
		{"unknown blocker", func(s *chat.IssueSplit) { s.Edges = []chat.IssueSplitEdge{{Dependent: 1, Blocker: 3}} }, false},
		{"unknown child", func(s *chat.IssueSplit) { s.Edges = []chat.IssueSplitEdge{{Dependent: 3, Blocker: 1}} }, false},
		{"negative index", func(s *chat.IssueSplit) { s.Edges = []chat.IssueSplitEdge{{Dependent: 1, Blocker: -1}} }, false},
		{"duplicate edge", func(s *chat.IssueSplit) {
			s.Edges = []chat.IssueSplitEdge{{Dependent: 2, Blocker: 1}, {Dependent: 2, Blocker: 1}}
		}, false},
		{"empty", func(s *chat.IssueSplit) { s.Children = nil }, false},
		{"maximum children", func(s *chat.IssueSplit) {
			for len(s.Children) < coordinatorSplitMaxChildren {
				s.Children = append(s.Children, s.Children[0])
			}
		}, true},
		{"too many children", func(s *chat.IssueSplit) {
			for len(s.Children) <= coordinatorSplitMaxChildren {
				s.Children = append(s.Children, s.Children[0])
			}
		}, false},
		{"long title", func(s *chat.IssueSplit) { s.Children[0].Title = strings.Repeat("t", coordinatorProposalTitleBytes+1) }, false},
		{"long description", func(s *chat.IssueSplit) {
			s.Children[0].Description = strings.Repeat("x", coordinatorProposalBodyRunes+1)
		}, false},
		{"missing title", func(s *chat.IssueSplit) { s.Children[0].Title = " " }, false},
		{"missing description", func(s *chat.IssueSplit) { s.Children[0].Description = "" }, false},
		{"missing state", func(s *chat.IssueSplit) { s.Children[0].State = "" }, false},
		{"invalid priority", func(s *chat.IssueSplit) { value := 4; s.Children[0].Priority = &value }, false},
		{"non-native parent", func(s *chat.IssueSplit) { s.ParentID = "#1" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			split := chat.IssueSplit{ParentID: "wi_parent", Children: []chat.IssueSplitChild{{Title: "Storage", Description: "Build storage", State: "Todo"}, {Title: "API", Description: "Build API", State: "Todo"}}}
			test.change(&split)
			err := validateCoordinatorIssueSplit(split)
			if (err == nil) != test.valid {
				t.Fatalf("validation=%v, want valid=%v", err, test.valid)
			}
			if err != nil {
				var native *nativeError
				if !errors.As(err, &native) {
					t.Fatalf("validation error = %v, want native validation", err)
				}
				result := coordinatorToolError(fmt.Errorf("split arguments: %w", err))
				var feedback struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal([]byte(result.Content), &feedback); err != nil {
					t.Fatal(err)
				}
				if result.Success || feedback.Error != native.Message {
					t.Fatalf("validation feedback = %+v, want %q", result, native.Message)
				}
			}
		})
	}
}
