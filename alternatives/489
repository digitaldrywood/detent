package auth

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/emailtemplate"
)

const smtpTimeout = 10 * time.Second

type EmailMessage = emailtemplate.Message

type InlineImage = emailtemplate.InlineImage

type EmailSender interface {
	SendEmail(context.Context, EmailMessage) error
}

type SMTPSender interface {
	Sender
	EmailSender
}

type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
}

type smtpSender struct {
	host        string
	port        int
	username    string
	password    string
	from        string
	timeout     time.Duration
	implicitTLS bool
	tlsConfig   *tls.Config
}

func NewSMTPSender(cfg SMTPConfig) (SMTPSender, error) {
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
		host:        host,
		port:        cfg.Port,
		username:    strings.TrimSpace(cfg.Username),
		password:    cfg.Password,
		from:        from.Address,
		timeout:     smtpTimeout,
		implicitTLS: cfg.Port == 465,
		tlsConfig:   &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host},
	}, nil
}

func (s *smtpSender) SendMagicLink(ctx context.Context, message Message) error {
	email, err := emailtemplate.Render(emailtemplate.Content{
		Title:       "Sign in to Detent",
		Note:        "Use this button to sign in. This link can be used once and expires at " + message.ExpiresAt.UTC().Format("Jan 2, 2006 15:04:05 UTC") + ".",
		ActionLabel: "Sign in",
		ActionURL:   message.URL,
		Footer:      "If you didn't request this, you can ignore this email.",
	})
	if err != nil {
		return err
	}
	email.To = message.To
	email.Subject = "Your Detent sign-in link"
	email.Body = magicLinkBody(message)
	return s.SendEmail(ctx, email)
}

func (s *smtpSender) SendEmail(ctx context.Context, message EmailMessage) (err error) {
	if strings.ContainsAny(message.To+message.Subject, "\r\n") {
		return errors.New("email headers are invalid")
	}
	recipient, parseErr := mail.ParseAddress(strings.TrimSpace(message.To))
	if parseErr != nil || recipient == nil || recipient.Address == "" {
		return errors.New("email recipient is invalid")
	}
	data, err := emailMessage(s.from, recipient.Address, message)
	if err != nil {
		return fmt.Errorf("encode email: %w", err)
	}
	deadline := time.Now().Add(s.timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	dialer := net.Dialer{Deadline: deadline}
	address := net.JoinHostPort(s.host, strconv.Itoa(s.port))
	var connection net.Conn
	if s.implicitTLS {
		tlsDialer := tls.Dialer{NetDialer: &dialer, Config: s.tlsConfig}
		connection, err = tlsDialer.DialContext(ctx, "tcp", address)
	} else {
		connection, err = dialer.DialContext(ctx, "tcp", address)
	}
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

	if ok, _ := client.Extension("STARTTLS"); ok && !s.implicitTLS {
		if err := client.StartTLS(s.tlsConfig); err != nil {
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
	if _, err := writer.Write(data); err != nil {
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

func magicLinkBody(message Message) string {
	return "Use this link to sign in to Detent:\r\n\r\n" + message.URL + "\r\n\r\nThis link can be used once and expires at " + message.ExpiresAt.UTC().Format(time.RFC3339) + ".\r\n"
}

func emailMessage(from string, to string, message EmailMessage) ([]byte, error) {
	headers := "From: " + from + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + mime.QEncoding.Encode("utf-8", message.Subject) + "\r\n" +
		"MIME-Version: 1.0\r\n"
	body := strings.ReplaceAll(strings.ReplaceAll(message.Body, "\r\n", "\n"), "\n", "\r\n")
	if message.HTMLBody == "" {
		return []byte(headers + "Content-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" + body), nil
	}
	var buffer bytes.Buffer
	alternative := multipart.NewWriter(&buffer)
	buffer.WriteString(headers + "Content-Type: " + mime.FormatMediaType("multipart/alternative", map[string]string{"boundary": alternative.Boundary()}) + "\r\n\r\n")
	if err := writeEmailPart(alternative, "text/plain", message.Body); err != nil {
		return nil, err
	}
	if len(message.InlineImages) == 0 {
		if err := writeEmailPart(alternative, "text/html", message.HTMLBody); err != nil {
			return nil, err
		}
	} else {
		var relatedBody bytes.Buffer
		related := multipart.NewWriter(&relatedBody)
		if err := writeEmailPart(related, "text/html", message.HTMLBody); err != nil {
			return nil, err
		}
		for _, image := range message.InlineImages {
			if image.ContentID == "" || strings.ContainsAny(image.ContentID+image.ContentType, "\r\n<>") {
				return nil, errors.New("inline image headers are invalid")
			}
			part, err := related.CreatePart(textproto.MIMEHeader{
				"Content-Type":              {image.ContentType},
				"Content-ID":                {"<" + image.ContentID + ">"},
				"Content-Disposition":       {"inline"},
				"Content-Transfer-Encoding": {"base64"},
			})
			if err != nil {
				return nil, err
			}
			encoded := base64.StdEncoding.EncodeToString(image.Data)
			for len(encoded) > 0 {
				line := min(76, len(encoded))
				if _, err := io.WriteString(part, encoded[:line]+"\r\n"); err != nil {
					return nil, err
				}
				encoded = encoded[line:]
			}
		}
		if err := related.Close(); err != nil {
			return nil, err
		}
		part, err := alternative.CreatePart(textproto.MIMEHeader{"Content-Type": {mime.FormatMediaType("multipart/related", map[string]string{"boundary": related.Boundary(), "type": "text/html"})}})
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(relatedBody.Bytes()); err != nil {
			return nil, err
		}
	}
	if err := alternative.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func writeEmailPart(writer *multipart.Writer, contentType, body string) error {
	part, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {contentType + "; charset=UTF-8"},
		"Content-Transfer-Encoding": {"quoted-printable"},
	})
	if err != nil {
		return err
	}
	encoded := quotedprintable.NewWriter(part)
	if _, err := io.WriteString(encoded, body); err != nil {
		return errors.Join(err, encoded.Close())
	}
	return encoded.Close()
}
