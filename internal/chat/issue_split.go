package chat

import "encoding/json"

const ActionIssueSplit ActionKind = "propose_issue_split"

type IssueSplitChild struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Priority    *int   `json:"priority,omitempty"`
	State       string `json:"state"`
}

type IssueSplitEdge struct {
	Dependent int `json:"dependent"`
	Blocker   int `json:"blocker"`
}

type IssueSplit struct {
	ParentID string            `json:"parent_work_item_id"`
	Children []IssueSplitChild `json:"children"`
	Edges    []IssueSplitEdge  `json:"edges"`
	Revision int64             `json:"expected_revision,omitempty"`
}

func (a Action) IssueSplit() *IssueSplit {
	if a.Kind != ActionIssueSplit {
		return nil
	}
	var split IssueSplit
	if json.Unmarshal(a.Arguments, &split) != nil {
		return nil
	}
	return &split
}
