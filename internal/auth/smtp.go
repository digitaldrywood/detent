package auth

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

const smtpTimeout = 10 * time.Second

type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
}

type smtpSender struct {
	host     string
	port     int
	username string
	password string
	from     string
	timeout  time.Duration
}

func NewSMTPSender(cfg SMTPConfig) (Sender, error) {
	host := strings.TrimSpace(cfg.Host)
	if host == "" {
		return nil, errors.New("smtp host is required")
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return nil, errors.New("smtp port must be between 1 and 65535")
	}
	from, err := mail.ParseAddress(strings.TrimSpace(cfg.From))
	if err != nil || from == nil || from.Address == "" {
		return nil, errors.New("smtp from address is invalid")
	}
	if (strings.TrimSpace(cfg.Username) == "") != (cfg.Password == "") {
		return nil, errors.New("smtp username and password must be set together")
	}
	return &smtpSender{
		host:     host,
		port:     cfg.Port,
		username: strings.TrimSpace(cfg.Username),
		password: cfg.Password,
		from:     from.Address,
		timeout:  smtpTimeout,
	}, nil
}

func (s *smtpSender) SendMagicLink(ctx context.Context, message Message) (err error) {
	recipient, parseErr := mail.ParseAddress(strings.TrimSpace(message.To))
	if parseErr != nil || recipient == nil || recipient.Address == "" {
		return errors.New("magic link recipient is invalid")
	}
	deadline := time.Now().Add(s.timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	dialer := net.Dialer{Deadline: deadline}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(s.host, strconv.Itoa(s.port)))
	if err != nil {
		return fmt.Errorf("connect smtp server: %w", err)
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return errors.Join(fmt.Errorf("set smtp deadline: %w", err), connection.Close())
	}
	client, err := smtp.NewClient(connection, s.host)
	if err != nil {
		return errors.Join(fmt.Errorf("create smtp client: %w", err), connection.Close())
	}
	defer func() {
		if client != nil {
			err = errors.Join(err, client.Close())
		}
	}()

	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{MinVersion: tls.VersionTLS12, ServerName: s.host}); err != nil {
			return fmt.Errorf("start smtp tls: %w", err)
		}
	}
	if s.username != "" {
		if err := client.Auth(smtp.PlainAuth("", s.username, s.password, s.host)); err != nil {
			return fmt.Errorf("authenticate smtp client: %w", err)
		}
	}
	if err := client.Mail(s.from); err != nil {
		return fmt.Errorf("set smtp sender: %w", err)
	}
	if err := client.Rcpt(recipient.Address); err != nil {
		return fmt.Errorf("set smtp recipient: %w", err)
	}
	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("start smtp message: %w", err)
	}
	if _, err := writer.Write(magicLinkMessage(s.from, recipient.Address, message)); err != nil {
		return errors.Join(fmt.Errorf("write smtp message: %w", err), writer.Close())
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finish smtp message: %w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("quit smtp client: %w", err)
	}
	client = nil
	return nil
}

func magicLinkMessage(from string, to string, message Message) []byte {
	body := "Use this link to sign in to Detent:\r\n\r\n" + message.URL + "\r\n\r\nThis link can be used once and expires at " + message.ExpiresAt.UTC().Format(time.RFC3339) + ".\r\n"
	headers := "From: " + from + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: Your Detent sign-in link\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n" +
		"Content-Transfer-Encoding: 8bit\r\n\r\n"
	return []byte(headers + body)
}
