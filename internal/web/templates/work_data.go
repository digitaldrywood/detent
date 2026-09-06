package templates

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

type workItemMetadata struct {
	Key              string
	Identity         string
	Search           string
	State            string
	StateKey         string
	Project          string
	Priority         string
	PriorityKey      string
	PriorityRank     int
	Readiness        string
	ReadinessKey     string
	ReadinessDetail  string
	ReadinessKind    primitives.Kind
	Machine          string
	MachineKey       string
	LeaseAge         string
	LeaseTitle       string
	PullRequest      string
	PullRequestURL   string
	PullRequestKey   string
	PullRequestTitle string
	Sync             string
	SyncKey          string
	SyncTitle        string
	SyncKind         primitives.Kind
	Updated          string
	UpdatedTitle     string
	UpdatedUnix      int64
	StageAge         string
	StageAgeTitle    string
	BlockerCount     int
}

type workItemView struct {
	Card boardCardView
	Meta workItemMetadata
}

type workFilterOption struct {
	Value string
	Label string
	Count int
}

func workItemViewFromCard(card boardCardView) workItemView {
	return workItemView{Card: card, Meta: card.Work}
}

func workListSheetOpenAttrs(projectID string, issueIdentity string, scope string) templ.Attributes {
	attrs := sheetOpenAttrs(projectID, issueIdentity, scope, true)
	attrs["hx-trigger"] = "click[!event.target.closest('a,button,input,select,label')]"
	return attrs
}

func workHealthTitle(data DashboardData) string {
	if snapshotDegraded(data.Snapshot) {
		return snapshotReadinessDetail(data.Snapshot)
	}
	return refreshFreshnessSummary(data.Snapshot)
}

func workItemMetadataFromCard(data DashboardData, card projectKanbanCard, view boardCardView) workItemMetadata {
	now := pipelineNow(data.Snapshot)
	readiness, readinessKey, readinessDetail, readinessKind := workItemReadiness(card, view)
	priority, priorityKey, priorityRank := workItemPriority(card)
	machine := strings.TrimSpace(card.Owner)
	if machine == "" {
		machine = "Unclaimed"
	}
	leaseAge, leaseTitle := workItemLease(card, now)
	syncLabel, syncKey, syncTitle, syncKind := workItemSync(data, card)
	updated, updatedTitle, updatedUnix := workItemUpdated(card, now)
	pullRequest := "No PR"
	pullRequestKey := "unlinked"
	pullRequestTitle := "No linked pull request"
	if view.PRStatus != "" {
		pullRequest = view.PRStatus
		pullRequestKey = "linked"
		pullRequestTitle = view.PRStatus
	}
	metadata := workItemMetadata{
		Key:              boardCardScopedSlug(view.Project, view.Identity),
		Identity:         view.Identity,
		State:            card.Stage,
		StateKey:         projectKanbanLaneID(card.Stage),
		Project:          view.Project,
		Priority:         priority,
		PriorityKey:      priorityKey,
		PriorityRank:     priorityRank,
		Readiness:        readiness,
		ReadinessKey:     readinessKey,
		ReadinessDetail:  readinessDetail,
		ReadinessKind:    readinessKind,
		Machine:          machine,
		MachineKey:       strings.ToLower(machine),
		LeaseAge:         leaseAge,
		LeaseTitle:       leaseTitle,
		PullRequest:      pullRequest,
		PullRequestURL:   view.PRURL,
		PullRequestKey:   pullRequestKey,
		PullRequestTitle: pullRequestTitle,
		Sync:             syncLabel,
		SyncKey:          syncKey,
		SyncTitle:        syncTitle,
		SyncKind:         syncKind,
		Updated:          updated,
		UpdatedTitle:     updatedTitle,
		UpdatedUnix:      updatedUnix,
		StageAge:         card.TimeInStage,
		StageAgeTitle:    card.TimeInStageTitle,
		BlockerCount:     len(card.Blockers),
	}
	metadata.Search = strings.ToLower(strings.Join([]string{
		view.Identity,
		view.Number,
		view.Title,
		view.Project,
		card.Stage,
		priority,
		readiness,
		readinessDetail,
		machine,
		syncLabel,
		strings.Join(card.Labels, " "),
		strings.Join(card.Assignees, " "),
	}, " "))
	return metadata
}

func workItemReadiness(card projectKanbanCard, view boardCardView) (string, string, string, primitives.Kind) {
	switch {
	case view.Terminal:
		return "Complete", "complete", "Terminal workflow state", primitives.KindOK
	case view.Running:
		return "Running", "running", firstNonBlank(view.RuntimeCozyText, "Active machine lease"), primitives.KindOK
	case view.ExtraChip && view.ExtraKind == primitives.KindErr:
		return "Blocked", "blocked", firstNonBlank(view.ExtraText, card.BlockedReason, "Operator attention required"), primitives.KindErr
	case len(card.Blockers) > 0:
		return "Waiting", "waiting", firstNonBlank(view.ExtraText, "Waiting on "+card.Blockers[0]), primitives.KindWarn
	case view.Retrying:
		return "Waiting", "waiting", firstNonBlank(view.ExtraText, "Awaiting retry"), primitives.KindInfo
	case strings.EqualFold(strings.TrimSpace(card.Stage), "Todo"):
		return "Ready", "ready", "Dispatchable when capacity is available", primitives.KindInfo
	case strings.EqualFold(strings.TrimSpace(card.Stage), "Backlog"):
		return "Waiting", "waiting", "Not in a dispatch lane", primitives.KindNeutral
	default:
		return "Waiting", "waiting", firstNonBlank(view.ExtraText, card.WaitDetail, "No live attempt"), primitives.KindNeutral
	}
}

func workItemPriority(card projectKanbanCard) (string, string, int) {
	name := strings.ToLower(strings.TrimSpace(card.PriorityName))
	rank := card.PriorityRank
	if (rank <= 0 || rank > 4) && card.DispatchPriorityRank > 0 && card.DispatchPriorityRank <= 4 {
		rank = card.DispatchPriorityRank
	}
	if rank <= 0 || rank > 4 {
		switch name {
		case "urgent", "p0":
			rank = 1
		case "high", "p1":
			rank = 2
		case "normal", "medium", "p2":
			rank = 3
		case "low", "p3":
			rank = 4
		default:
			rank = 5
		}
	}
	if name == "" {
		name = strings.ToLower(strings.TrimSpace(card.DispatchPriorityLabel))
	}
	if name == "" && card.UnblockerCount > 0 {
		name = "prerequisite"
	}
	if name == "" {
		return "Unset", "unset", rank
	}
	key := "unset"
	switch rank {
	case 1:
		key = "urgent"
	case 2:
		key = "high"
	case 3:
		key = "normal"
	case 4:
		key = "low"
	}
	return strings.ToUpper(name[:1]) + name[1:], key, rank
}

func workItemLease(card projectKanbanCard, now time.Time) (string, string) {
	if card.LeaseRenewedAt == nil {
		return "", "No active lease timestamp"
	}
	age := prPipelineAge(*card.LeaseRenewedAt, now)
	label := "renewed " + age + " ago"
	title := "Lease renewed " + timeLabel(*card.LeaseRenewedAt)
	if card.LeaseExpiresAt != nil {
		title += "; expires " + timeLabel(*card.LeaseExpiresAt)
	}
	return label, title
}

func workItemSync(data DashboardData, card projectKanbanCard) (string, string, string, primitives.Kind) {
	if projectKanbanCardKanbanData(data, card).TrackerKind == "hub_native" {
		return "Native", "native", "Authoritative Detent issue", primitives.KindNeutral
	}
	key := strings.ToLower(strings.TrimSpace(card.SyncStatus))
	if key == "" {
		if data.Snapshot.LastKnown || snapshotDegraded(data.Snapshot) {
			key = "stale"
		} else {
			key = "synced"
		}
	}
	lastSync := ""
	if syncedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(card.SourceSyncedAt)); err == nil {
		lastSync = "; last synchronized " + timeLabel(syncedAt)
	}
	switch key {
	case "pending":
		return "Pending", key, "GitHub write-back is pending" + lastSync, primitives.KindInfo
	case "retrying":
		return "Retrying", key, "GitHub write-back is retrying" + lastSync, primitives.KindWarn
	case "error":
		return "Error", key, "GitHub synchronization needs operator attention" + lastSync, primitives.KindErr
	case "stale":
		return "Stale", key, "GitHub projection is stale" + lastSync, primitives.KindWarn
	default:
		return "Synced", "synced", "GitHub projection is synchronized" + lastSync, primitives.KindOK
	}
}

func workItemUpdated(card projectKanbanCard, now time.Time) (string, string, int64) {
	updated := card.UpdatedAt
	if updated == nil && !card.StageAt.IsZero() {
		updated = &card.StageAt
	}
	if updated == nil || updated.IsZero() {
		return "Unknown", "Updated time is unavailable", 0
	}
	age := prPipelineAge(*updated, now)
	return age + " ago", "Updated " + timeLabel(*updated), updated.Unix()
}

func workStateFilterOptions(view boardView) []workFilterOption {
	options := make([]workFilterOption, 0, len(view.Lanes))
	for _, lane := range view.Lanes {
		options = append(options, workFilterOption{Value: lane.LaneID, Label: lane.Title, Count: lane.CardCount})
	}
	return options
}

func workProjectFilterOptions(view boardView) []workFilterOption {
	counts := map[string]int{}
	for _, item := range view.Items {
		counts[item.Meta.Project]++
	}
	options := make([]workFilterOption, 0, len(counts))
	for project, count := range counts {
		if strings.TrimSpace(project) == "" {
			continue
		}
		options = append(options, workFilterOption{Value: strings.ToLower(project), Label: project, Count: count})
	}
	sort.Slice(options, func(i, j int) bool {
		return strings.ToLower(options[i].Label) < strings.ToLower(options[j].Label)
	})
	return options
}

func workNewIssueURL(data DashboardData) string {
	if !isProjectDashboard(data) {
		return ""
	}
	if data.Kanban.TrackerKind == "hub_native" {
		return NativeNewIssuePath(data.ProjectID)
	}
	base := strings.TrimRight(projectRepoURL(data), "/")
	base = strings.TrimSuffix(base, "/issues")
	if base == "" {
		return ""
	}
	return base + "/issues/new"
}

func workItemDataAttributes(meta workItemMetadata, representation string) templ.Attributes {
	return templ.Attributes{
		"data-work-item":           "true",
		"data-work-representation": representation,
		"data-work-key":            meta.Key,
		"data-work-identity":       meta.Identity,
		"data-work-project":        strings.ToLower(meta.Project),
		"data-work-state":          meta.StateKey,
		"data-work-priority":       meta.PriorityKey,
		"data-work-priority-rank":  strconv.Itoa(meta.PriorityRank),
		"data-work-readiness":      meta.ReadinessKey,
		"data-work-machine":        meta.MachineKey,
		"data-work-pr":             meta.PullRequestKey,
		"data-work-sync":           meta.SyncKey,
		"data-work-updated":        strconv.FormatInt(meta.UpdatedUnix, 10),
		"data-work-search":         meta.Search,
	}
}

func workSyncBadgeClass(meta workItemMetadata) string {
	return "inline-flex items-center gap-1.5 whitespace-nowrap " + boardExtraTextClass(meta.SyncKind)
}

func workReadinessClass(meta workItemMetadata) string {
	return "inline-flex min-w-0 items-center gap-1.5 " + boardExtraTextClass(meta.ReadinessKind)
}

func workBlockerLabel(count int) string {
	if count == 0 {
		return "No blockers"
	}
	return boardCountLabel(count, "blocker", "blockers")
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
