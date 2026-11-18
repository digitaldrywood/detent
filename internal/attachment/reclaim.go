package attachment

import "time"

type ReclaimCandidate struct {
	Key      string     `json:"key"`
	Revision int64      `json:"revision"`
	Due      *time.Time `json:"due,omitempty"`
}

type ReclaimPage struct {
	After   string             `json:"after"`
	Changed []ReclaimCandidate `json:"changed"`
}

type ReclaimReceipt struct {
	After   string             `json:"after"`
	Next    string             `json:"next"`
	Checked []ReclaimCandidate `json:"checked"`
}

func (r ReclaimReceipt) Valid(organization string) bool {
	if len(r.Checked) > ReferenceBatchSize {
		return false
	}
	for _, cursor := range []string{r.After, r.Next} {
		if cursor != "" && !(ObjectLookup{Key: cursor}).Valid(organization) {
			return false
		}
	}
	for _, candidate := range r.Checked {
		if candidate.Revision <= 0 || candidate.Key == "" || !(ObjectLookup{Key: candidate.Key}).Valid(organization) {
			return false
		}
	}
	return true
}
