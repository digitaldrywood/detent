package templates

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/digitaldrywood/detent/internal/connector"
)

type IssueDetailData struct {
	Dashboard   DashboardData
	Identifier  string
	Number      string
	Title       string
	Description string
	State       string
	Priority    string
	Labels      []string
	Assignees   []string
	Fields      []IssueDetailField
	Events      []IssueDetailEvent
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type IssueDetailField struct {
	Name  string
	Value string
}

type IssueDetailEvent struct {
	ID     string
	Title  string
	Detail string
	Body   string
	At     time.Time
}

func NewIssueDetailData(dashboard DashboardData, issue connector.Issue, events []connector.IssueEvent) IssueDetailData {
	return IssueDetailData{
		Dashboard:   dashboard,
		Identifier:  strings.TrimSpace(issue.Identifier),
		Number:      issueDetailNumber(issue.Number),
		Title:       strings.TrimSpace(issue.Title),
		Description: issue.Description,
		State:       strings.TrimSpace(issue.State),
		Priority:    issueDetailPriority(issue),
		Labels:      uniqueStrings(issue.Labels),
		Assignees:   uniqueStrings(issue.Assignees),
		Fields:      issueDetailFields(issue.Fields),
		Events:      issueDetailEvents(events),
		CreatedAt:   timeValue(issue.CreatedAt),
		UpdatedAt:   timeValue(issue.UpdatedAt),
	}
}

func issueDetailShellData(data IssueDetailData) DashboardShellData {
	shell := DashboardShellDataFromDashboard(data.Dashboard)
	shell.Title = data.Title + " - Detent"
	shell.ActiveNav = "project"
	return shell
}

func issueDetailProjectPath(data IssueDetailData) string {
	projectID := strings.TrimSpace(data.Dashboard.ProjectID)
	if projectID == "" {
		return "/"
	}
	return "/projects/" + url.PathEscape(projectID) + "/kanban"
}

func issueDetailReference(data IssueDetailData) string {
	if data.Number != "" {
		return data.Number + " · " + data.Identifier
	}
	return data.Identifier
}

func issueDetailNumber(number int) string {
	if number <= 0 {
		return ""
	}
	return "#" + strconv.Itoa(number)
}

func issueDetailPriority(issue connector.Issue) string {
	if name := strings.TrimSpace(issue.PriorityName); name != "" {
		return name
	}
	if issue.Priority == nil {
		return ""
	}
	return strconv.Itoa(*issue.Priority)
}

func issueDetailFields(fields map[string]string) []IssueDetailField {
	out := make([]IssueDetailField, 0, len(fields))
	for name, value := range fields {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, IssueDetailField{Name: name, Value: strings.TrimSpace(value)})
		}
	}
	sort.Slice(out, func(i int, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func issueDetailEvents(events []connector.IssueEvent) []IssueDetailEvent {
	out := make([]IssueDetailEvent, 0, len(events))
	for _, event := range events {
		view := IssueDetailEvent{
			ID:   strings.TrimSpace(event.ID),
			Body: strings.TrimSpace(event.Body),
			At:   timeValue(event.CreatedAt),
		}
		switch strings.TrimSpace(event.Kind) {
		case "comment":
			view.Title = "Comment"
		case "comment_edit":
			view.Title = "Comment edited"
		case "comment_delete":
			view.Title = "Comment deleted"
			view.Body = strings.TrimSpace(event.Fields["previous_body"])
		case "state_update":
			view.Title = "State changed"
			view.Detail = strings.TrimSpace(event.State)
		case "field_update":
			view.Title = "Fields updated"
			view.Detail = issueDetailEventFields(event.Fields)
		case "close":
			view.Title = "Issue closed"
			view.Detail = strings.TrimSpace(event.State)
		case "project_remove":
			view.Title = "Removed from project"
		default:
			view.Title = issueDetailEventTitle(event.Kind)
			view.Detail = strings.TrimSpace(event.State)
		}
		out = append(out, view)
	}
	return out
}

func issueDetailEventFields(fields map[string]string) string {
	rows := issueDetailFields(fields)
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Name == "actor" || row.Name == "comment_id" || row.Name == "previous_body" {
			continue
		}
		if row.Value == "" {
			parts = append(parts, row.Name+" cleared")
			continue
		}
		parts = append(parts, row.Name+" → "+row.Value)
	}
	return strings.Join(parts, ", ")
}

func issueDetailEventTitle(kind string) string {
	kind = strings.ReplaceAll(strings.TrimSpace(kind), "_", " ")
	if kind == "" {
		return "Issue updated"
	}
	runes := []rune(kind)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

func timeValue(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return value.UTC()
}
