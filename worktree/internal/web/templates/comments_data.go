package templates

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	kanbanConversationTargetIssue       = "issue"
	kanbanConversationTargetPullRequest = "pull_request"
)

type KanbanConversationData struct {
	ProjectID           string
	BoardScope          string
	IssueIdentity       string
	IssueID             string
	Identifier          string
	Title               string
	IssueURL            string
	PRRepository        string
	PRNumber            int
	PRLabel             string
	PRURL               string
	BoardActions        bool
	Expanded            bool
	CanComment          bool
	PRCommentsSupported bool
	IssuePending        bool
	PRPending           bool
	IssueComments       []KanbanConversationComment
	PRComments          []KanbanConversationComment
	IssueError          string
	PRError             string
	IssueNotice         string
	Body                string
}

type KanbanConversationComment struct {
	ID             string
	Body           string
	URL            string
	Author         string
	Created        string
	CreatedTitle   string
	Backend        string
	LocationStatus string
	CanEdit        bool
	CanDelete      bool
}

func BoardCardConversationData(data DashboardData, card projectKanbanCard, boardActions bool, expanded bool) KanbanConversationData {
	projectID := projectKanbanCardProjectID(data, card)
	return KanbanConversationData{
		ProjectID:           projectID,
		BoardScope:          projectKanbanBoardScope(data),
		IssueIdentity:       boardCardIdentityToken(card.Identifier, card.IssueID, card.IssueNumber),
		IssueID:             strings.TrimSpace(card.IssueID),
		Identifier:          strings.TrimSpace(card.Identifier),
		Title:               strings.TrimSpace(card.Title),
		IssueURL:            strings.TrimSpace(card.URL),
		PRRepository:        strings.TrimSpace(card.PRRepository),
		PRNumber:            card.PRNumber,
		PRLabel:             strings.TrimSpace(card.PullRequestLabel),
		PRURL:               strings.TrimSpace(card.PRURL),
		BoardActions:        boardActions,
		Expanded:            expanded,
		CanComment:          boardActions && projectKanbanCardCanComment(data, card) && strings.TrimSpace(card.IssueID) != "",
		IssueComments:       kanbanConversationComments(card.Comments, kanbanConversationTargetIssue),
		PRComments:          kanbanConversationComments(card.Comments, kanbanConversationTargetPullRequest),
		PRCommentsSupported: kanbanConversationHasSnapshotPRComments(card.Comments),
	}
}

func KanbanConversationWithIssueComments(data KanbanConversationData, comments []telemetry.IssueComment, message string) KanbanConversationData {
	data.IssueComments = kanbanConversationComments(comments, kanbanConversationTargetIssue)
	data.IssueError = strings.TrimSpace(message)
	return data
}

func KanbanConversationWithPRComments(data KanbanConversationData, comments []telemetry.IssueComment, supported bool, message string) KanbanConversationData {
	data.PRComments = kanbanConversationComments(comments, kanbanConversationTargetPullRequest)
	data.PRCommentsSupported = supported
	data.PRError = strings.TrimSpace(message)
	return data
}

func KanbanConversationWithIssueForm(data KanbanConversationData, body string, message string, notice string) KanbanConversationData {
	data.Body = strings.TrimSpace(body)
	data.IssueError = strings.TrimSpace(message)
	data.IssueNotice = strings.TrimSpace(notice)
	return data
}

func kanbanConversationComments(comments []telemetry.IssueComment, target string) []KanbanConversationComment {
	out := make([]KanbanConversationComment, 0, len(comments))
	for _, comment := range comments {
		if !kanbanConversationCommentMatchesTarget(comment, target) {
			continue
		}
		out = append(out, KanbanConversationComment{
			ID:             strings.TrimSpace(comment.ID),
			Body:           strings.TrimSpace(comment.Body),
			URL:            strings.TrimSpace(comment.URL),
			Author:         kanbanConversationCommentAuthor(comment),
			Created:        kanbanConversationCommentCreated(comment.CreatedAt),
			CreatedTitle:   kanbanConversationCommentCreatedTitle(comment.CreatedAt),
			Backend:        kanbanConversationCommentBackend(comment.Backend),
			LocationStatus: kanbanConversationCommentLocation(comment.Local),
			CanEdit:        target == kanbanConversationTargetIssue && comment.Local && comment.CanEdit && strings.TrimSpace(comment.ID) != "",
			CanDelete:      target == kanbanConversationTargetIssue && comment.Local && comment.CanDelete && strings.TrimSpace(comment.ID) != "",
		})
	}
	return out
}

func kanbanConversationCommentMatchesTarget(comment telemetry.IssueComment, target string) bool {
	target = strings.TrimSpace(target)
	commentTarget := strings.TrimSpace(comment.TargetType)
	if commentTarget == "" {
		commentTarget = kanbanConversationTargetIssue
	}
	return commentTarget == target
}

func kanbanConversationHasSnapshotPRComments(comments []telemetry.IssueComment) bool {
	for _, comment := range comments {
		if strings.TrimSpace(comment.TargetType) == kanbanConversationTargetPullRequest {
			return true
		}
	}
	return false
}

func kanbanConversationCommentAuthor(comment telemetry.IssueComment) string {
	if value := strings.TrimSpace(comment.AuthorDisplayName); value != "" {
		return value
	}
	if value := strings.TrimSpace(comment.AuthorLogin); value != "" {
		return value
	}
	return "Unknown author"
}

func kanbanConversationCommentCreated(value *time.Time) string {
	if value == nil || value.IsZero() {
		return "time unavailable"
	}
	return timeLabel(value.UTC())
}

func kanbanConversationCommentCreatedTitle(value *time.Time) string {
	if value == nil || value.IsZero() {
		return ""
	}
	return localTimeISOString(*value)
}

func kanbanConversationCommentBackend(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "github":
		return "GitHub"
	case "github_local":
		return "GitHub Local"
	case "local_sqlite":
		return "Local"
	case "memory":
		return "Memory"
	default:
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
		return "Tracker"
	}
}

func kanbanConversationCommentLocation(local bool) string {
	if local {
		return "local"
	}
	return "remote"
}

func kanbanConversationHasPRTab(data KanbanConversationData) bool {
	return data.PRCommentsSupported && strings.TrimSpace(data.PRRepository) != "" && data.PRNumber > 0
}

func kanbanConversationCanMutateComment(data KanbanConversationData, comment KanbanConversationComment) bool {
	return data.BoardActions &&
		data.CanComment &&
		strings.TrimSpace(data.IssueID) != "" &&
		(comment.CanEdit || comment.CanDelete)
}

func kanbanConversationCanEditComment(data KanbanConversationData, comment KanbanConversationComment) bool {
	return kanbanConversationCanMutateComment(data, comment) && comment.CanEdit
}

func kanbanConversationCommentDeletePath(data KanbanConversationData, comment KanbanConversationComment) string {
	values := url.Values{}
	addQueryValue(values, "project_id", data.ProjectID)
	addQueryValue(values, "issue_id", data.IssueID)
	addQueryValue(values, "comment_id", comment.ID)
	return "/api/v1/kanban/comment?" + values.Encode()
}

func kanbanConversationCountLabel(count int) string {
	if count == 1 {
		return "1"
	}
	return strconv.Itoa(count)
}

func kanbanBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func kanbanTabIndex(active bool) string {
	if active {
		return "0"
	}
	return "-1"
}

func kanbanConversationExpandLabel(data KanbanConversationData) string {
	if data.Expanded {
		return "Collapse discussion"
	}
	return "Expand discussion"
}

func kanbanCommentListClass(expanded bool) string {
	base := "dt-lane-scroll flex flex-col gap-2 overflow-y-auto rounded-card border border-line bg-page p-2"
	if expanded {
		return base + " max-h-[60vh] min-h-64"
	}
	return base + " max-h-96 min-h-28"
}

func kanbanConversationSheetPath(data KanbanConversationData, expanded bool) string {
	path := boardCardSheetPath(data.ProjectID, data.IssueIdentity, data.BoardScope, data.BoardActions)
	if expanded {
		path += "&expanded=1"
	}
	return path
}

func kanbanConversationLoadPath(data KanbanConversationData, target string) string {
	path := boardCardDetailPath("/api/v1/board/conversation", data.ProjectID, data.IssueIdentity, data.BoardScope, data.BoardActions)
	path += "&target=" + url.QueryEscape(strings.TrimSpace(target))
	if data.Expanded {
		path += "&expanded=1"
	}
	return path
}

func kanbanConversationPRLabel(data KanbanConversationData) string {
	if label := strings.TrimSpace(data.PRLabel); label != "" {
		return label
	}
	if data.PRNumber > 0 {
		return "PR #" + strconv.Itoa(data.PRNumber)
	}
	return "PR"
}
