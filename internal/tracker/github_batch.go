package tracker

// GitHubDiscovery describes one explicit, read-only source page.
type GitHubDiscovery struct {
	Repository    string   `json:"repository"`
	IncludeClosed bool     `json:"include_closed"`
	Labels        []string `json:"labels"`
	Cursor        string   `json:"cursor"`
}

type GitHubIssuePreview struct {
	Number int      `json:"number"`
	ID     string   `json:"id"`
	URL    string   `json:"url"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Closed bool     `json:"closed"`
	Labels []string `json:"labels"`
}

type GitHubDiscoveryPage struct {
	Issues     []GitHubIssuePreview `json:"issues"`
	Total      int                  `json:"total"`
	NextCursor string               `json:"next_cursor"`
}

type GitHubBatchItem struct {
	Number     int              `json:"number"`
	WorkItemID NativeWorkItemID `json:"work_item_id,omitempty"`
	Status     string           `json:"status"`
	Error      string           `json:"error,omitempty"`
	RetryAt    string           `json:"retry_at,omitempty"`
}

// GitHubBatch persists intake intent and checkpoints, without execution state.
type GitHubBatch struct {
	ID          string              `json:"id"`
	Revision    int64               `json:"revision"`
	RunnerID    string              `json:"runner_id"`
	Discovery   GitHubDiscovery     `json:"discovery"`
	Page        GitHubDiscoveryPage `json:"page"`
	Status      string              `json:"status"`
	Error       string              `json:"error,omitempty"`
	RetryAt     string              `json:"retry_at,omitempty"`
	Destination string              `json:"destination"`
	Items       []GitHubBatchItem   `json:"items"`
}

type GitHubBatchCommand struct {
	Mutation
	Revision      int64    `json:"revision"`
	Action        string   `json:"action"`
	RunnerID      string   `json:"runner_id"`
	Labels        []string `json:"labels"`
	IncludeClosed bool     `json:"include_closed"`
	Numbers       []int    `json:"numbers"`
	Destination   string   `json:"destination"`
	AllowDispatch bool     `json:"allow_dispatch"`
}

type GitHubBatchTask struct {
	ProjectID ProjectID           `json:"project_id"`
	BatchID   string              `json:"batch_id"`
	Revision  int64               `json:"revision"`
	Discovery *GitHubDiscovery    `json:"discovery,omitempty"`
	Item      *GitHubIssuePreview `json:"item,omitempty"`
}

type GitHubBatchResult struct {
	Mutation
	BatchID  string               `json:"batch_id"`
	Revision int64                `json:"revision"`
	Page     *GitHubDiscoveryPage `json:"page,omitempty"`
	Snapshot *GitHubIssueSnapshot `json:"snapshot,omitempty"`
	Number   int                  `json:"number"`
	Error    string               `json:"error,omitempty"`
	RetryAt  string               `json:"retry_at,omitempty"`
}
