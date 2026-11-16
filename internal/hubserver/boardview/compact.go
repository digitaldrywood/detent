package boardview

import (
	"cmp"
	"encoding/json"
	"slices"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Lanes struct {
	Preset  string   `json:"preset"`
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

func (l Lanes) Selects(state tracker.NativeState) bool {
	switch {
	case l.Preset == viewstore.LanesOpen && state.Terminal, l.Preset == viewstore.LanesActive && (state.Terminal || !state.Dispatchable):
		return false
	case len(l.Include) > 0 && !slices.Contains(l.Include, state.Name):
		return false
	}
	return !slices.Contains(l.Exclude, state.Name)
}

func (l Lanes) Unknown(projects []Project) string {
	known := map[string]bool{}
	for _, project := range projects {
		for _, state := range project.Project.States {
			known[state.Name] = true
		}
	}
	for _, name := range slices.Concat(l.Include, l.Exclude) {
		if !known[name] {
			return name
		}
	}
	return ""
}

type CompactLane struct {
	State        string `json:"state"`
	Terminal     bool   `json:"terminal"`
	Dispatchable bool   `json:"dispatchable"`
	Selected     bool   `json:"selected"`
	Total        int    `json:"total"`
	Running      int    `json:"running"`
	Returned     int    `json:"returned"`
}

type CompactShipped struct {
	Window    string `json:"window"`
	Completed int    `json:"completed"`
	Cancelled int    `json:"cancelled"`
}

type CompactProject struct {
	ID                        tracker.ProjectID `json:"id"`
	Name                      string            `json:"name"`
	Slug                      string            `json:"slug"`
	Sequence                  int64             `json:"sequence"`
	ProjectionPending         bool              `json:"projection_pending"`
	ProjectionOldestPendingAt *time.Time        `json:"projection_oldest_pending_at,omitempty"`
	CountsAsOf                *time.Time        `json:"counts_as_of,omitempty"`
	Shipped                   []CompactShipped  `json:"shipped"`
	Lanes                     []CompactLane     `json:"lanes"`
}

type CompactAttempt struct {
	Count     int        `json:"count"`
	Status    string     `json:"status"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	Model     string     `json:"model,omitempty"`
}

type CompactExecution struct {
	Running   bool       `json:"running"`
	Stage     string     `json:"stage,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
}

type CompactChange struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type CompactCard struct {
	ProjectID       tracker.ProjectID        `json:"project_id"`
	ID              tracker.NativeWorkItemID `json:"id"`
	Identifier      string                   `json:"identifier"`
	Number          int                      `json:"number"`
	Title           string                   `json:"title"`
	State           string                   `json:"state"`
	Type            string                   `json:"type,omitempty"`
	Priority        *int                     `json:"priority,omitempty"`
	Labels          []string                 `json:"labels,omitempty"`
	Assignees       []string                 `json:"assignees,omitempty"`
	Archived        bool                     `json:"archived,omitempty"`
	Live            bool                     `json:"live"`
	Attempt         *CompactAttempt          `json:"attempt,omitempty"`
	Execution       *CompactExecution        `json:"execution,omitempty"`
	DependencyReady bool                     `json:"dependency_ready"`
	BlockedBy       int                      `json:"blocked_by"`
	OpenBlockers    int                      `json:"open_blockers"`
	Unblocks        int                      `json:"unblocks"`
	Change          *CompactChange           `json:"change,omitempty"`
	CreatedAt       time.Time                `json:"created_at"`
	UpdatedAt       time.Time                `json:"updated_at"`
	LastActivityAt  time.Time                `json:"last_activity_at"`
	ClosedAt        *time.Time               `json:"closed_at,omitempty"`
}

type CompactContinuation struct {
	State      string `json:"state"`
	Returned   int    `json:"returned"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type CompactBoard struct {
	OrganizationID tracker.OrganizationID `json:"organization_id"`
	GeneratedAt    time.Time              `json:"generated_at"`
	Lanes          Lanes                  `json:"lanes"`
	Projects       []CompactProject       `json:"projects"`
	Unavailable    []tracker.ProjectID    `json:"unavailable_projects"`
	Truncated      bool                   `json:"truncated"`
	Continuations  []CompactContinuation  `json:"continuations"`
	Cards          []CompactCard          `json:"cards"`
}

func compactCard(card tracker.NativeBoardCard, running bool) CompactCard {
	issue := card.Issue
	out := CompactCard{ProjectID: issue.ProjectID, ID: issue.WorkItemID, Identifier: Identifier(issue.ProjectID, issue.Number), Number: issue.Number, Title: issue.Title, State: issue.State, Type: issue.Type, Priority: issue.Priority,
		Labels: issue.Labels, Assignees: issue.Assignees, Archived: issue.Archived, Live: running, DependencyReady: issue.DependencyReady, BlockedBy: len(issue.Blockers), Unblocks: len(issue.Dependents),
		CreatedAt: issue.CreatedAt, UpdatedAt: issue.UpdatedAt, LastActivityAt: issue.LastActivityAt, ClosedAt: issue.ClosedAt}
	for _, blocker := range issue.Blockers {
		if !blocker.Terminal {
			out.OpenBlockers++
		}
	}
	if attempt := card.Attempt; attempt != nil {
		out.Attempt = &CompactAttempt{Count: attempt.Count, Status: attempt.Status}
		if !attempt.StartedAt.IsZero() {
			out.Attempt.StartedAt = &attempt.StartedAt
		}
		if attempt.Identity != nil {
			out.Attempt.Model = attempt.Identity.Model
		}
	}
	if execution := card.Execution; execution != nil {
		out.Execution = &CompactExecution{Running: execution.Running, Stage: execution.Stage, StartedAt: execution.StartedAt}
		out.Live = out.Live || execution.Running
	}
	if card.Change != nil {
		out.Change = &CompactChange{ID: card.Change.ID, Status: card.Change.Status}
	}
	return out
}

func Identifier(project tracker.ProjectID, number int) string {
	return string(project) + "#" + strconv.Itoa(number)
}

func compactOrder(a, b CompactCard) int {
	closed := func(card CompactCard) int64 {
		if card.ClosedAt == nil {
			return 0
		}
		return card.ClosedAt.UnixNano()
	}
	return cmp.Or(cmp.Compare(closed(b), closed(a)), cmp.Compare(b.Identifier, a.Identifier))
}

func compactLanes(project Project, lanes Lanes, returned map[tracker.ProjectID]map[string]int) []CompactLane {
	counts := map[string]tracker.NativeWorkLane{}
	if project.Work != nil {
		for _, lane := range project.Work.Lanes {
			counts[lane.State] = lane
		}
	}
	out := make([]CompactLane, 0, len(project.Project.States))
	for _, state := range project.Project.States {
		count := counts[state.Name]
		out = append(out, CompactLane{State: state.Name, Terminal: state.Terminal, Dispatchable: state.Dispatchable, Selected: lanes.Selects(state), Total: count.Total, Running: count.Running, Returned: returned[project.Project.ID][state.Name]})
	}
	return out
}

func Compact(organization tracker.OrganizationID, now time.Time, view View, windows []Cursor, lanes Lanes, unavailable []tracker.ProjectID, budget int) (CompactBoard, []Cursor, error) {
	board := CompactBoard{OrganizationID: organization, GeneratedAt: now, Lanes: lanes, Projects: []CompactProject{}, Unavailable: unavailable, Continuations: []CompactContinuation{}, Cards: []CompactCard{}}
	if board.Unavailable == nil {
		board.Unavailable = []tracker.ProjectID{}
	}
	byLane := map[string][]CompactCard{}
	var names []string
	for _, card := range view.Cards {
		compact := compactCard(card, view.Running[card.Issue.WorkItemID])
		if _, found := byLane[compact.State]; !found {
			names = append(names, compact.State)
		}
		byLane[compact.State] = append(byLane[compact.State], compact)
	}
	slices.Sort(names)
	for _, name := range names {
		slices.SortFunc(byLane[name], compactOrder)
	}
	returned := map[tracker.ProjectID]map[string]int{}
	build := func() {
		board.Projects = board.Projects[:0]
		for _, project := range view.Projects {
			entry := CompactProject{ID: project.Project.ID, Name: project.Project.Name, Slug: project.Project.Slug, Sequence: project.Sequence, ProjectionPending: project.ProjectionPending, ProjectionOldestPendingAt: project.ProjectionOldestPendingAt, Shipped: []CompactShipped{}, Lanes: compactLanes(project, lanes, returned)}
			if work := project.Work; work != nil {
				entry.CountsAsOf = &work.AsOf
				for _, shipped := range work.ShippedWindows {
					entry.Shipped = append(entry.Shipped, CompactShipped{Window: shipped.Window, Completed: shipped.Shipped, Cancelled: shipped.Cancelled})
				}
			}
			board.Projects = append(board.Projects, entry)
		}
	}
	build()
	envelope, err := json.Marshal(board)
	if err != nil {
		return board, nil, err
	}
	used := len(envelope) + len(names)*96
	taken := map[string]int{}
	for progress := true; progress; {
		progress = false
		for _, name := range names {
			if taken[name] == len(byLane[name]) {
				continue
			}
			raw, err := json.Marshal(byLane[name][taken[name]])
			if err != nil {
				return board, nil, err
			}
			if used+len(raw)+1 > budget && taken[name] > 0 {
				continue
			}
			used += len(raw) + 1
			card := byLane[name][taken[name]]
			board.Cards = append(board.Cards, card)
			taken[name]++
			if returned[card.ProjectID] == nil {
				returned[card.ProjectID] = map[string]int{}
			}
			returned[card.ProjectID][name]++
			progress = true
		}
	}
	build()
	windowed := map[string]Cursor{}
	for _, position := range windows {
		windowed[position.State] = position
	}
	var cursors []Cursor
	for _, name := range names {
		list := byLane[name]
		continuation := CompactContinuation{State: name, Returned: taken[name]}
		position, more := windowed[name]
		switch {
		case taken[name] < len(list):
			last := list[taken[name]-1]
			position, more = Cursor{State: name, More: true, ClosedAt: last.ClosedAt, Identifier: last.Identifier}, true
			board.Truncated = true
		case more && !position.More:
			more = false
		}
		board.Continuations = append(board.Continuations, continuation)
		if more {
			cursors = append(cursors, position)
		}
	}
	slices.SortStableFunc(board.Cards, func(a, b CompactCard) int { return cmp.Or(cmp.Compare(a.State, b.State), compactOrder(a, b)) })
	return board, cursors, nil
}
