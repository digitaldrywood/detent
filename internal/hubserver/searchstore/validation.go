package searchstore

func (s Statistics) Validate() error {
	if s.Tokens < 0 || len(s.Terms) != len(s.Frequencies) {
		return ErrInvalid
	}
	for i, term := range s.Terms {
		if term == "" || s.Frequencies[i] <= 0 || i > 0 && s.Terms[i-1] >= term {
			return ErrInvalid
		}
	}
	return nil
}

func (d Document) Validate() error {
	if d.OrganizationID == "" || d.ID == "" || d.SourceID == "" || d.Revision < 1 || d.UpdatedAt.IsZero() {
		return ErrInvalid
	}
	switch d.Kind {
	case "issue", "comment", "event", "change":
		if d.Scope != "issue" || d.Status != "" {
			return ErrInvalid
		}
	case "thread":
		if d.Status != "open" && d.Status != "resolved" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	switch d.Scope {
	case "general":
		if d.ProjectID != "" || d.WorkItemID != "" {
			return ErrInvalid
		}
	case "project":
		if d.ProjectID == "" || d.WorkItemID != "" {
			return ErrInvalid
		}
	case "issue":
		if d.ProjectID == "" || d.WorkItemID == "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	if d.Kind == "issue" && d.SourceID != string(d.WorkItemID) {
		return ErrInvalid
	}
	return nil
}

func (q Query) Validate() error {
	if q.Kind != "work" && q.Kind != "thread" || q.Kind == "work" && q.ProjectID == "" || len(q.ProjectIDs) > 100 || q.Reporter != nil && (*q.Reporter == "" || q.Kind == "thread") {
		return ErrInvalid
	}
	switch q.Scope {
	case "", "general", "project", "issue":
	default:
		return ErrInvalid
	}
	switch q.Status {
	case "", "open", "resolved":
	default:
		return ErrInvalid
	}
	switch q.Archived {
	case "", "false", "true", "all":
	default:
		return ErrInvalid
	}
	return nil
}
