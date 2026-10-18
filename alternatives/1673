package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const issueEmailWindow = time.Minute

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

type issueEmailNotice struct {
	StateFrom string `json:"state_from,omitempty"`
	StateTo   string `json:"state_to,omitempty"`
	Author    string `json:"author,omitempty"`
	CommentID string `json:"comment_id,omitempty"`
	Related   bool   `json:"related,omitempty"`
	Verify    bool   `json:"verify,omitempty"`
}

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
	var failures error
	for range 100 {
		message, found, err := s.claimIssueEmail(ctx, now)
		if err != nil {
			return errors.Join(failures, err)
		}
		if !found {
			break
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
	return failures
}

func (s *Service) collectIssueEmails(ctx context.Context) error {
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT e.work_item_id,e.project_id,e.sequence,e.type,e.actor_json,e.data_json,e.recorded_at
FROM collaboration_events e JOIN issues i ON i.native_id=e.work_item_id
JOIN projects p ON p.id=e.project_id LEFT JOIN issue_notification_cursors c ON c.work_item_id=e.work_item_id
WHERE e.organization_id=? AND p.profile='native' AND i.event_sequence>COALESCE(c.sequence,0) AND e.sequence>COALESCE(c.sequence,0)
ORDER BY e.rowid LIMIT 256`, s.config.Hosted.OrganizationID)
	if err != nil {
		return err
	}
	defer rows.Close()
	events := []issueEmailEvent{}
	for rows.Next() {
		var event issueEmailEvent
		var actor, data, at string
		if err := rows.Scan(&event.item, &event.project, &event.sequence, &event.kind, &actor, &data, &at); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(actor), &event.actor); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(data), &event.data); err != nil {
			rows.Close()
			return err
		}
		event.at, err = parseTimeValue(at)
		if err != nil {
			rows.Close()
			return err
		}
		events = append(events, event)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, event := range events {
		notice, notify := issueNotice(event)
		if notify {
			if notice.CommentID != "" {
				var author string
				err := tx.QueryRowContext(ctx, "SELECT email FROM hosted_members WHERE principal_id=?", event.actor.PrincipalID).Scan(&author)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if author != "" {
					notice.Author = author
				}
			}
			if err := s.queueIssueEmail(ctx, tx, event, event.item, notice); err != nil {
				return err
			}
			if notice.Related {
				if err := s.queueIssueEmail(ctx, tx, event, string(event.data.RelatedWorkItemID), notice); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO issue_notification_cursors(work_item_id,sequence) VALUES (?,?)
ON CONFLICT(work_item_id) DO UPDATE SET sequence=excluded.sequence`, event.item, event.sequence); err != nil {
			return err
		}
	}
	return tx.Commit()
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

func (s *Service) queueIssueEmail(ctx context.Context, tx *sql.Tx, event issueEmailEvent, item string, notice issueEmailNotice) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT m.user_id, m.principal_id=json_extract(i.actor_json,'$.principal_id') AND json_extract(i.actor_json,'$.kind')='human' FROM issues i JOIN hosted_members m
ON m.principal_id IN (
 SELECT json_extract(i.actor_json,'$.principal_id') WHERE json_extract(i.actor_json,'$.kind')='human'
 UNION SELECT json_extract(e.actor_json,'$.principal_id') FROM collaboration_events e
 WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id
 AND e.type='comment.created' AND json_extract(e.actor_json,'$.kind')='human'
 AND julianday(e.recorded_at)<=julianday(?) AND (i.native_id<>? OR e.sequence<=?)
)
JOIN api_tokens t ON t.id=m.principal_id
WHERE i.native_id=? AND i.organization_id=? AND m.active=1 AND m.notify_issues=1 AND t.revoked_at IS NULL
AND (?<>'human' OR m.principal_id<>?)
AND EXISTS(SELECT 1 FROM hosted_project_grants g WHERE g.user_id=m.user_id AND g.organization_id=i.organization_id AND g.project_id=i.project_id)`,
		formatHubTime(event.at), event.item, event.sequence, item, s.config.Hosted.OrganizationID, event.actor.Kind, event.actor.PrincipalID)
	if err != nil {
		return err
	}
	defer rows.Close()
	type recipient struct {
		user  string
		filer bool
	}
	users := []recipient{}
	for rows.Next() {
		var user recipient
		if err := rows.Scan(&user.user, &user.filer); err != nil {
			rows.Close()
			return err
		}
		users = append(users, user)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, recipient := range users {
		personal := notice
		personal.Verify = notice.Verify && recipient.filer && item == event.item
		raw, err := json.Marshal(personal)
		if err != nil {
			return err
		}
		user := recipient.user
		if _, err := tx.ExecContext(ctx, `INSERT INTO issue_subscriptions(work_item_id,user_id,token) VALUES (?,?,?) ON CONFLICT(work_item_id,user_id) DO NOTHING`, item, user, newNativeID("unsub")); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO issue_notification_batches(work_item_id,user_id,due_at,notices_json)
SELECT work_item_id,user_id,?,json_array(json(?)) FROM issue_subscriptions WHERE work_item_id=? AND user_id=? AND unsubscribed=0
ON CONFLICT(work_item_id,user_id) DO UPDATE SET notices_json=json_insert(issue_notification_batches.notices_json,'$[#]',json(?))`,
			formatHubTime(event.at.Add(issueEmailWindow)), string(raw), item, user, string(raw)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) claimIssueEmail(ctx context.Context, now time.Time) (auth.EmailMessage, bool, error) {
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return auth.EmailMessage{}, false, err
	}
	defer tx.Rollback()
	var item, user, raw string
	err = tx.QueryRowContext(ctx, `DELETE FROM issue_notification_batches WHERE (work_item_id,user_id) IN
(SELECT work_item_id,user_id FROM issue_notification_batches WHERE julianday(due_at)<=julianday(?) ORDER BY due_at LIMIT 1)
RETURNING work_item_id,user_id,notices_json`, formatHubTime(now)).Scan(&item, &user, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.EmailMessage{}, false, nil
	}
	if err != nil {
		return auth.EmailMessage{}, false, err
	}
	var message auth.EmailMessage
	var title, token, verification string
	var number int
	err = tx.QueryRowContext(ctx, `SELECT m.email,i.title,i.number,s.token,COALESCE(i.verification_state,'') FROM issue_subscriptions s
JOIN hosted_members m ON m.user_id=s.user_id JOIN api_tokens t ON t.id=m.principal_id JOIN issues i ON i.native_id=s.work_item_id
WHERE s.work_item_id=? AND s.user_id=? AND s.unsubscribed=0 AND m.notify_issues=1 AND m.active=1 AND t.revoked_at IS NULL AND i.organization_id=?
AND EXISTS(SELECT 1 FROM hosted_project_grants g WHERE g.user_id=m.user_id AND g.organization_id=i.organization_id AND g.project_id=i.project_id)`, item, user, s.config.Hosted.OrganizationID).Scan(&message.To, &title, &number, &token, &verification)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return auth.EmailMessage{}, false, err
	}
	if err == nil {
		var notices []issueEmailNotice
		if err := json.Unmarshal([]byte(raw), &notices); err != nil {
			return auth.EmailMessage{}, false, err
		}
		if verification != tracker.VerificationPending {
			for i := range notices {
				notices[i].Verify = false
			}
		}
		path, err := readIssueBrowserPath(ctx, tx, item)
		if err != nil {
			return auth.EmailMessage{}, false, err
		}
		message = s.renderIssueEmail(message.To, strings.TrimRight(s.config.Hosted.PublicURL, "/")+path, title, number, token, notices)
	}
	if err := tx.Commit(); err != nil {
		return auth.EmailMessage{}, false, err
	}
	return message, true, nil
}

func (s *Service) renderIssueEmail(to, link, title string, number int, token string, notices []issueEmailNotice) auth.EmailMessage {
	var body strings.Builder
	subject := fmt.Sprintf("[Detent] #%d %s", number, strings.NewReplacer("\r", " ", "\n", " ").Replace(title))
	if slices.ContainsFunc(notices, func(notice issueEmailNotice) bool { return notice.Verify }) {
		subject = "[Detent] Please verify #" + strconv.Itoa(number) + ": " + strings.NewReplacer("\r", " ", "\n", " ").Replace(title)
		fmt.Fprintf(&body, "Please verify #%d. Your issue is Done. Open it and choose Verified, or Still broken with a comment: %s\n\n", number, link)
	}
	fmt.Fprintf(&body, "#%d %s\n\n", number, title)
	for _, notice := range notices {
		switch {
		case notice.CommentID != "":
			fmt.Fprintf(&body, "%s commented: %s#issue-comment-%s\n", notice.Author, link, url.PathEscape(notice.CommentID))
		case notice.Related:
			fmt.Fprintf(&body, "A related issue link was added: %s\n", link)
		default:
			fmt.Fprintf(&body, "%s to %s\n", notice.StateFrom, notice.StateTo)
		}
	}
	fmt.Fprintf(&body, "\nOpen issue: %s\nUnsubscribe from this issue: %s\n", link, s.config.Hosted.PublicURL+s.hostedPath("/issue-notifications/unsubscribe?token="+url.QueryEscape(token)))
	return auth.EmailMessage{To: to, Subject: subject, Body: body.String()}
}
