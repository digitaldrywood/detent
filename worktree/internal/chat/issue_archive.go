package chat

import "encoding/json"

const ActionArchiveItems ActionKind = "archive_items"

type IssueArchiveItem struct {
	WorkItemID string `json:"work_item_id"`
	Revision   int64  `json:"expected_revision"`
	Number     int    `json:"number"`
	Title      string `json:"title"`
	State      string `json:"state"`
}

type IssueArchive struct {
	Items []IssueArchiveItem `json:"items"`
}

func (a Action) IssueArchive() *IssueArchive {
	if a.Kind != ActionArchiveItems {
		return nil
	}
	var archive IssueArchive
	if json.Unmarshal(a.Arguments, &archive) != nil {
		return nil
	}
	return &archive
}
