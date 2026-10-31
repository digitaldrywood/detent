package cloudentry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/emailtemplate"
)

type platformNotifications struct {
	sender auth.EmailSender
	wake   chan struct{}
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *Service) startPlatformNotifications(parent context.Context) error {
	key := os.Getenv("RESEND_API_KEY")
	if key == "" {
		s.config.Logger.Info("platform notifications skipped: RESEND_API_KEY is not set")
		return nil
	}
	sender := s.config.platformEmailSender
	if sender == nil {
		var err error
		sender, err = auth.NewSMTPSender(auth.SMTPConfig{
			Host: "smtp.resend.com", Port: 465, Username: "resend", Password: key,
			From: "Detent <notifications@mail.detent.build>",
		})
		if err != nil {
			return fmt.Errorf("configure platform notifications: %w", err)
		}
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	n := &platformNotifications{sender: sender, wake: make(chan struct{}, 1), cancel: cancel, done: make(chan struct{})}
	s.notifications = n
	go func() {
		defer close(n.done)
		for {
			s.deliverPlatformNotifications(ctx)
			select {
			case <-ctx.Done():
				return
			case <-n.wake:
			}
		}
	}()
	return nil
}

func (s *Service) closePlatformNotifications() {
	if n := s.notifications; n != nil {
		n.cancel()
		<-n.done
	}
}

func (s *Service) wakePlatformNotifications() {
	if n := s.notifications; n != nil {
		select {
		case n.wake <- struct{}{}:
		default:
		}
	}
}

func enqueuePlatformNotification(ctx context.Context, tx *sql.Tx, event, id, email, name, plan, at string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO platform_notifications(event,entity_id,email,name,plan,occurred_at)
VALUES(?,?,?,?,?,?) ON CONFLICT(event,entity_id) DO NOTHING`, event, id, email, name, plan, at)
	return err
}

func (s *Service) recordSignIn(ctx context.Context, identity auth.Identity) error {
	tx, err := s.auth.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := formatTime(s.config.now())
	if s.notifications != nil && identity.Hosted.SupportActor == "" {
		var earlier bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM audit WHERE subject=? AND event='session_started')", identity.Subject).Scan(&earlier); err != nil {
			return err
		}
		if !earlier {
			if err := enqueuePlatformNotification(ctx, tx, "signup", identity.Subject, identity.Email, "", "", at); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO audit(subject,organization_id,event,recorded_at) VALUES (?,?,'session_started',?)", identity.Subject, "", at); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.wakePlatformNotifications()
	return nil
}

func (s *Service) deliverPlatformNotifications(ctx context.Context) {
	for _, source := range []*store{s.auth.store, s.registry.store} {
		for ctx.Err() == nil {
			var event, id, email, name, plan, at string
			err := source.db.QueryRowContext(ctx, `UPDATE platform_notifications SET attempted_at=?
WHERE (event,entity_id)=(SELECT event,entity_id FROM platform_notifications WHERE attempted_at IS NULL ORDER BY occurred_at LIMIT 1)
RETURNING event,entity_id,email,name,plan,occurred_at`, formatTime(s.config.now())).Scan(&event, &id, &email, &name, &plan, &at)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				if ctx.Err() == nil {
					s.config.Logger.ErrorContext(ctx, "claim platform notification failed", "error", err)
				}
				break
			}
			message, err := s.platformNotificationMessage(event, id, email, name, plan, at)
			if err != nil {
				s.config.Logger.ErrorContext(ctx, "render platform notification failed", "event", event, "error", err)
				continue
			}
			if err := s.sendPlatformNotification(ctx, message); err != nil {
				if errors.Is(err, context.Canceled) {
					s.config.Logger.InfoContext(ctx, "platform notification delivery interrupted", "event", event, "error", err)
					continue
				}
				s.config.Logger.ErrorContext(ctx, "platform notification delivery failed", "event", event, "error", err)
			}
		}
	}
}

func (s *Service) platformNotificationMessage(event, id, email, name, plan, at string) (auth.EmailMessage, error) {
	occurredAt, err := parseTime(at)
	if err != nil {
		return auth.EmailMessage{}, fmt.Errorf("parse notification time: %w", err)
	}
	formattedTime := occurredAt.UTC().Format("Jan 2, 2006 15:04 UTC")
	content := emailtemplate.Content{
		Title: "New sign-up", Heading: notificationLine(email),
		Details:     []emailtemplate.Detail{{Label: "Email", Value: notificationLine(email)}, {Label: "Time", Value: formattedTime}},
		ActionLabel: "Open user", ActionURL: s.config.PublicURL + "/platform/users/" + url.PathEscape(email),
		Footer: "You get this because you are a Detent platform admin.",
	}
	if event != "signup" {
		content.Title = "New organization"
		content.Heading = notificationLine(name)
		content.Details = []emailtemplate.Detail{
			{Label: "Organization", Value: notificationLine(name)},
			{Label: "Creator", Value: notificationLine(email)},
			{Label: "Plan", Value: plan},
			{Label: "Time", Value: formattedTime},
		}
		content.ActionLabel = "Open organization"
		content.ActionURL = s.config.PublicURL + "/platform/tenants/" + url.PathEscape(id)
	}
	message, err := emailtemplate.Render(content)
	if err != nil {
		return auth.EmailMessage{}, err
	}
	message.Subject = content.Title + ": " + content.Heading
	return message, nil
}

func (s *Service) sendPlatformNotification(ctx context.Context, message auth.EmailMessage) error {
	rows, err := s.registry.store.reader.QueryContext(ctx, "SELECT email FROM platform_members WHERE role='admin' ORDER BY email")
	if err != nil {
		return err
	}
	defer rows.Close()
	var recipients []string
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return err
		}
		recipients = append(recipients, email)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	var sendErr error
	for _, recipient := range recipients {
		message.To = recipient
		sendErr = errors.Join(sendErr, s.notifications.sender.SendEmail(ctx, message))
	}
	return sendErr
}

func notificationLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func (s *Service) platformNotificationPlan(price string) string {
	plan := "free"
	runtime := s.config.Runtime
	if current := s.currentRuntime(); current != nil {
		runtime = &current.Update
	}
	if runtime != nil && runtime.Plans != nil && runtime.Plans.Base.ID != "" {
		plan = runtime.Plans.Base.ID
	}
	for _, configured := range s.allocationPrices() {
		if configured.PriceID == price {
			return configured.Plan.ID
		}
	}
	return plan
}
