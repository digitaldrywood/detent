package tracker

import (
	"encoding/json"
	"errors"
	"time"
)

type PullRequestMergeable struct {
	Known bool
	Value bool
}

func (m PullRequestMergeable) MarshalJSON() ([]byte, error) {
	if !m.Known {
		return []byte(`"unknown"`), nil
	}
	return json.Marshal(m.Value)
}

func (m *PullRequestMergeable) UnmarshalJSON(data []byte) error {
	var value bool
	if err := json.Unmarshal(data, &value); err == nil {
		m.Known, m.Value = true, value
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil || text != "unknown" {
		return errors.New(`mergeable must be true, false or "unknown"`)
	}
	m.Known, m.Value = false, false
	return nil
}

type PullRequestRef struct {
	Ref        string `json:"ref"`
	SHA        string `json:"sha,omitempty"`
	Repository string `json:"repository,omitempty"`
}

type PullRequestCheck struct {
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion"`
	URL         string     `json:"url"`
	CompletedAt *time.Time `json:"completed_at"`
}

type PullRequestReview struct {
	Author      string    `json:"author"`
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submitted_at"`
}

type PullRequestConnector struct {
	Provider       string    `json:"provider"`
	Repository     string    `json:"repository"`
	SynchronizedAt time.Time `json:"synchronized_at"`
}

// PullRequestView is one row of the panel: a change request, joined with the
// connector's projection of its pull request when there is one.
type PullRequestView struct {
	ID             string                `json:"id"`
	ChangeID       string                `json:"change_id"`
	Number         int                   `json:"number"`
	Title          string                `json:"title"`
	State          string                `json:"state"`
	Draft          bool                  `json:"draft"`
	URL            string                `json:"url"`
	Head           PullRequestRef        `json:"head"`
	Base           PullRequestRef        `json:"base"`
	Author         string                `json:"author"`
	FromFork       bool                  `json:"from_fork"`
	Mergeable      PullRequestMergeable  `json:"mergeable"`
	Checks         []PullRequestCheck    `json:"checks"`
	Reviews        []PullRequestReview   `json:"reviews"`
	ReviewDecision string                `json:"review_decision"`
	Labels         []string              `json:"labels"`
	UpdatedAt      time.Time             `json:"updated_at"`
	FetchedAt      time.Time             `json:"fetched_at"`
	Connector      *PullRequestConnector `json:"connector"`
}
