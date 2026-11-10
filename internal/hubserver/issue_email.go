package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/emailtemplate"
	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type issueMailer struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type issueEmailEvent struct {
	item, project, kind string
	sequence            int64
	actor               tracker.Actor
	data                tracker.CollaborationData
	at                  time.Time
}

type issueEmailNotice = accountstore.IssueNotice

func (s *Service) startIssueMailer() {
	if s.config.Hosted == nil || s.config.Hosted.EmailSender == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.issueMailer = &issueMailer{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.issueMailer.done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.deliverDailySummaries(ctx, s.config.now()); err != nil && ctx.Err() == nil {
					s.config.Logger.Warn("daily summary delivery failed", "error", err)
				}
				if err := s.deliverIssueEmails(ctx, s.config.now()); err != nil && ctx.Err() == nil {
					s.config.Logger.Warn("issue notification delivery failed", "error", err)
				}
			}
		}
	}()
}

func (s *Service) stopIssueMailer() {
	if s.issueMailer != nil {
		s.issueMailer.cancel()
		<-s.issueMailer.done
	}
}

func (s *Service) deliverIssueEmails(ctx context.Context, now time.Time) error {
	if s.config.Hosted == nil || s.config.Hosted.EmailSender == nil {
		return nil
	}
	if err := s.collectIssueEmails(ctx); err != nil {
		return err
	}
	repository, ok := s.config.AccessRepository.(accountstore.NotificationRepository)
	if !ok {
		return errors.New("notification repository unavailable")
	}
	seek, err := repository.NotificationSeek(ctx, s.accessOrganization())
	if err != nil {
		return err
	}
	var failures error
	cursor, wrapped, examined := seek, false, false
	var passed []accountstore.NotificationCandidate
	for range 100 {
		message, page, consumed, err := s.claimIssueEmail(ctx, now, cursor, passed)
		if !page.Found && !page.More {
			if err != nil {
				failures = errors.Join(failures, err)
				break
			}
			if wrapped || seek.DueAt.IsZero() {
				break
			}
			cursor, wrapped = accountstore.NotificationCursor{}, true
			continue
		}
		cursor, examined = page.Next, true
		if page.Found && !consumed {
			passed = append(passed, page.Candidate)
		}
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		if message.To == "" {
			continue
		}
		sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = s.config.Hosted.EmailSender.SendEmail(sendCtx, message)
		cancel()
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("send issue notification: %w", err))
		}
	}
	if examined {
		cursor.Revision = seek.Revision
		if _, err := repository.AdvanceNotificationSeek(ctx, s.accessOrganization(), cursor); err != nil {
			failures = errors.Join(failures, err)
		}
	}
	return failures
}

func (s *Service) collectIssueEmails(ctx context.Context) error {
	repository, ok := s.config.AccessRepository.(accountstore.NotificationRepository)
	if !ok {
		return errors.New("notification repository unavailable")
	}
	records, err := repository.NotificationEvents(ctx, s.accessOrganization())
	if err != nil {
		return err
	}
	budget := 256
	for _, record := range records {
		event, _, err := s.readTenantCollaborationEvent(ctx, record)
		if err != nil {
			return err
		}
		notice, notify := issueNotice(issueEmailEvent{item: string(record.WorkItemID), project: string(record.ProjectID), kind: record.Type, sequence: record.Sequence, actor: event.Actor, data: event.Data, at: record.RecordedAt})
		complete, count, err := repository.QueueNotification(ctx, s.accessOrganization(), record, notice, event.Data.RelatedWorkItemID, notify, budget)
		if err != nil {
			return err
		}
		budget -= count
		if !complete || budget == 0 {
			return nil
		}
	}
	return nil
}

func issueNotice(event issueEmailEvent) (issueEmailNotice, bool) {
	switch event.kind {
	case "workflow.transitioned":
		return issueEmailNotice{StateFrom: event.data.FromState, StateTo: event.data.ToState, Verify: event.data.Verification == tracker.VerificationPending}, event.data.FromState != event.data.ToState
	case "comment.created":
		return issueEmailNotice{Author: event.actor.PrincipalID, CommentID: event.data.CommentID}, event.data.CommentID != ""
	case "issue.edited":
		related := event.data.Operation == "add" && event.data.RelatedWorkItemID != "" && slices.Contains(event.data.Fields, "related")
		return issueEmailNotice{Related: related}, related
	}
	return issueEmailNotice{}, false
}

func (s *Service) claimIssueEmail(ctx context.Context, now time.Time, cursor accountstore.NotificationCursor, passed []accountstore.NotificationCandidate) (auth.EmailMessage, accountstore.NotificationPage, bool, error) {
	repository, ok := s.config.AccessRepository.(accountstore.NotificationRepository)
	if !ok {
		return auth.EmailMessage{}, accountstore.NotificationPage{}, false, errors.New("notification repository unavailable")
	}
	page, err := repository.DueNotification(ctx, s.accessOrganization(), now, cursor, passed)
	if err != nil || !page.Found {
		return auth.EmailMessage{}, page, false, err
	}
	notice, unresolved, claimed, err := repository.ClaimNotification(ctx, s.accessOrganization(), now, page.Candidate, false)
	if err == nil && !claimed && unresolved.Verify {
		var pending bool
		pending, unresolved.Revision, err = s.issueVerificationPending(ctx, unresolved.ProjectID, unresolved.WorkItemID)
		if err == nil {
			notice, _, claimed, err = repository.ClaimNotification(ctx, s.accessOrganization(), now, unresolved, pending)
		}
	}
	if err != nil || !claimed || notice.Email == "" {
		return auth.EmailMessage{}, page, err == nil && claimed, err
	}
	link := strings.TrimRight(s.config.Hosted.PublicURL, "/") + issueBrowserPath(notice.OrganizationSlug, notice.ProjectSlug, notice.Number)
	message, err := s.renderIssueEmail(notice.Email, link, notice.Title, notice.Number, notice.Token, notice.Notices)
	return message, page, true, err
}

func (s *Service) issueVerificationPending(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (bool, int64, error) {
	var row corestore.WorkItem
	var version eventstore.CollaborationVersion
	err := s.config.TenantRepository.ReadTenant(ctx, s.accessOrganization(), corestore.Background, func(r tenantstore.Reader) error {
		var err error
		row, err = r.WorkItem(ctx, project, item)
		if err != nil {
			return err
		}
		version, err = r.CollaborationVersion(ctx, string(item), row.Revision)
		return err
	})
	if err != nil {
		return false, 0, fmt.Errorf("read notification work item: %w", err)
	}
	issue, err := s.hydrateCanonicalWorkItemRecord(ctx, row, version)
	if err != nil {
		return false, 0, fmt.Errorf("read notification verification: %w", err)
	}
	return issue.Verification != nil && issue.Verification.State == tracker.VerificationPending, row.Revision, nil
}

func (s *Service) renderIssueEmail(to, link, title string, number int, token string, notices []issueEmailNotice) (auth.EmailMessage, error) {
	var body strings.Builder
	issue := emailtemplate.Issue{
		URL: link, Verify: slices.ContainsFunc(notices, func(notice issueEmailNotice) bool { return notice.Verify }),
		UnsubscribeURL: s.config.Hosted.PublicURL + s.hostedPath("/issue-notifications/unsubscribe?token="+url.QueryEscape(token)),
	}
	subject := fmt.Sprintf("[Detent] #%d %s", number, strings.NewReplacer("\r", " ", "\n", " ").Replace(title))
	if issue.Verify {
		subject = "[Detent] Please verify #" + strconv.Itoa(number) + ": " + strings.NewReplacer("\r", " ", "\n", " ").Replace(title)
		fmt.Fprintf(&body, "Please verify #%d. Your issue is Done. Open it and choose Verified, or Still broken with a comment: %s\n\n", number, link)
	}
	fmt.Fprintf(&body, "#%d %s\n\n", number, title)
	for _, notice := range notices {
		row := emailtemplate.Notice{StateFrom: notice.StateFrom, StateTo: notice.StateTo, Author: notice.Author, Related: notice.Related}
		switch {
		case notice.CommentID != "":
			row.CommentURL = link + "#issue-comment-" + url.PathEscape(notice.CommentID)
			fmt.Fprintf(&body, "%s commented: %s\n", notice.Author, row.CommentURL)
		case notice.Related:
			fmt.Fprintf(&body, "A related issue link was added: %s\n", link)
		default:
			fmt.Fprintf(&body, "%s to %s\n", notice.StateFrom, notice.StateTo)
		}
		issue.Notices = append(issue.Notices, row)
	}
	fmt.Fprintf(&body, "\nOpen issue: %s\nUnsubscribe from this issue: %s\n", link, issue.UnsubscribeURL)
	message, err := emailtemplate.Render(emailtemplate.Content{
		Title: fmt.Sprintf("Issue #%d", number), Heading: title, Issue: &issue,
		ActionLabel: "Open issue", ActionURL: link,
		Footer: "You get this because you filed or commented on this issue.",
	})
	if err != nil {
		return auth.EmailMessage{}, err
	}
	message.To, message.Subject, message.Body = to, subject, body.String()
	return message, nil
}
