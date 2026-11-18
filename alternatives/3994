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

func (q Query) Validate() error {
	if q.Kind != "work" || q.ProjectID == "" || len(q.Text) > MaxQueryTextBytes {
		return ErrInvalid
	}
	return nil
}
