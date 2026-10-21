package tracker

// GitHubDiscovery describes one explicit, read-only source page.
type GitHubDiscovery struct {
	ProjectV2     *GitHubProjectV2Source `json:"project_v2,omitempty"`
	Snapshots     bool                   `json:"snapshots,omitempty"`
	Repository    string                 `json:"repository"`
	IncludeClosed bool                   `json:"include_closed"`
	Labels        []string               `json:"labels"`
	Cursor        string                 `json:"cursor"`
}

type GitHubProjectV2Source struct {
	ID          string            `json:"id"`
	StatusLanes map[string]string `json:"status_lanes"`
}

type GitHubIssuePreview struct {
	ProjectStatus   string               `json:"project_status,omitempty"`
	Snapshot        *GitHubIssueSnapshot `json:"snapshot,omitempty"`
	DiscussionCount int                  `json:"discussion_count,omitempty"`
	NeedsDiscussion bool                 `json:"needs_discussion,omitempty"`
	Number          int                  `json:"number"`
	ID              string               `json:"id"`
	URL             string               `json:"url"`
	Title           string               `json:"title"`
	Body            string               `json:"body"`
	Closed          bool                 `json:"closed"`
	Labels          []string             `json:"labels"`
}

type GitHubDiscoveryPage struct {
	Issues     []GitHubIssuePreview `json:"issues"`
	Total      int                  `json:"total"`
	NextCursor string               `json:"next_cursor"`
}

type GitHubBatchItem struct {
	UnmappedProjectStatus *string          `json:"unmapped_project_status,omitempty"`
	Number                int              `json:"number"`
	WorkItemID            NativeWorkItemID `json:"work_item_id,omitempty"`
	Status                string           `json:"status"`
	Error                 string           `json:"error,omitempty"`
	RetryAt               string           `json:"retry_at,omitempty"`
	SourceCloseRequested  bool             `json:"source_close_requested,omitempty"`
}

// GitHubBatch persists intake intent and checkpoints, without execution state.
type GitHubBatch struct {
	RequestedBy   *Actor              `json:"requested_by,omitempty"`
	Automatic     bool                `json:"automatic,omitempty"`
	HubOwned      bool                `json:"hub_owned,omitempty"`
	AllowDispatch bool                `json:"allow_dispatch,omitempty"`
	LabelLanes    map[string]string   `json:"label_lanes,omitempty"`
	IgnoreLanes   bool                `json:"ignore_lane_labels,omitempty"`
	Limit         int                 `json:"limit,omitempty"`
	ID            string              `json:"id"`
	Revision      int64               `json:"revision"`
	RunnerID      string              `json:"runner_id"`
	Discovery     GitHubDiscovery     `json:"discovery"`
	Page          GitHubDiscoveryPage `json:"page"`
	Status        string              `json:"status"`
	Error         string              `json:"error,omitempty"`
	RetryAt       string              `json:"retry_at,omitempty"`
	Destination   string              `json:"destination"`
	Items         []GitHubBatchItem   `json:"items"`
}

type GitHubBatchCommand struct {
	Mutation
	ProjectV2     *GitHubProjectV2Source `json:"project_v2,omitempty"`
	LabelLanes    map[string]string      `json:"label_lanes,omitempty"`
	Revision      int64                  `json:"revision"`
	Action        string                 `json:"action"`
	RunnerID      string                 `json:"runner_id"`
	Labels        []string               `json:"labels"`
	IncludeClosed bool                   `json:"include_closed"`
	Numbers       []int                  `json:"numbers"`
	Destination   string                 `json:"destination"`
	AllowDispatch bool                   `json:"allow_dispatch"`
	IgnoreLanes   bool                   `json:"ignore_lane_labels"`
}

type GitHubBatchTask struct {
	Items     []GitHubIssuePreview `json:"items,omitempty"`
	ProjectID ProjectID            `json:"project_id"`
	BatchID   string               `json:"batch_id"`
	Revision  int64                `json:"revision"`
	Discovery *GitHubDiscovery     `json:"discovery,omitempty"`
	Item      *GitHubIssuePreview  `json:"item,omitempty"`
}

type GitHubBatchResult struct {
	Mutation
	Results  []GitHubBatchItemResult `json:"results,omitempty"`
	BatchID  string                  `json:"batch_id"`
	Revision int64                   `json:"revision"`
	Page     *GitHubDiscoveryPage    `json:"page,omitempty"`
	Snapshot *GitHubIssueSnapshot    `json:"snapshot,omitempty"`
	Number   int                     `json:"number"`
	Error    string                  `json:"error,omitempty"`
	RetryAt  string                  `json:"retry_at,omitempty"`
}

type GitHubBatchItemResult struct {
	Number   int                  `json:"number"`
	Snapshot *GitHubIssueSnapshot `json:"snapshot,omitempty"`
	Error    string               `json:"error,omitempty"`
	RetryAt  string               `json:"retry_at,omitempty"`
}

type GitHubBatchReceipt struct {
	Revision int64            `json:"revision"`
	Status   string           `json:"status"`
	Next     *GitHubBatchTask `json:"next,omitempty"`
}
