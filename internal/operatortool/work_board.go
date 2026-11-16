package operatortool

const WorkBoard = "work_board"

type WorkBoardArguments struct {
	OrganizationID  string   `json:"organization_id,omitempty"`
	ProjectIDs      []string `json:"project_ids,omitempty"`
	Lanes           string   `json:"lanes,omitempty"`
	Lane            []string `json:"lane,omitempty"`
	ExcludeLane     []string `json:"exclude_lane,omitempty"`
	Cursor          string   `json:"cursor,omitempty"`
	Archived        bool     `json:"archived,omitempty"`
	CompletedWindow string   `json:"completed_window,omitempty"`
}

func WorkBoardCatalog() []Definition {
	names := `{"type":"array","maxItems":64,"items":{"type":"string","minLength":1,"maxLength":256}}`
	d := definition(WorkBoard, "Read the whole native board of the organization in one statement, for auditing whether work is moving. project_ids narrows to readable projects (default: every project this credential can read; unreadable ids are listed in unavailable_projects). lanes selects by workflow flags: open (default, every non-terminal lane), active (dispatchable lanes) or all (adds the newest 20 cards of each terminal lane); lane includes only the named lanes and exclude_lane removes lanes. Lane counts are returned for every lane, selected or not. Cards are compact audit records: identifier, title, state, priority, type, labels, assignees, attempt count and status, live flag, blocker counts and change status. When the result would exceed the tool size limit, cards are taken from each lane in turn, truncated is true, and continuations[].next_cursor continues one lane: call again with lane=[that state] and cursor.",
		`{"type":"object","properties":{"organization_id":{"type":"string","maxLength":256},"project_ids":{"type":"array","maxItems":500,"items":{"type":"string","minLength":1,"maxLength":512}},"lanes":{"type":"string","enum":["open","active","all"]},"lane":`+names+`,"exclude_lane":`+names+`,"cursor":{"type":"string","maxLength":4096},"archived":{"type":"boolean"},"completed_window":{"type":"string","enum":["1h","24h","7d"]}},"additionalProperties":false}`)
	d.Annotations.OpenWorld = true
	return []Definition{d}
}
