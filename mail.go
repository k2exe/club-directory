package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Mailer struct {
	Host     string
	Port     int
	User     string
	Pass     string
	From     string
	StartTLS bool
	Insecure bool
	Outbox   string
	Echo     bool // also print to the log, for first-run and troubleshooting
}

func (m *Mailer) Configured() bool { return m.Host != "" }

// Send delivers a plain-text message. With no SMTP host configured it writes
// the message to the outbox directory instead, so a club can run the whole
// thing with no mail server at all and hand out links by other means.
func (m *Mailer) Send(to, subject, body string) error {
	msg := m.compose(to, subject, body)
	if m.Echo || !m.Configured() {
		log.Printf("mail to %s: %s\n%s", to, subject, indent(body))
	}
	if !m.Configured() {
		return m.spool(to, subject, msg)
	}
	if err := m.deliver(to, msg); err != nil {
		// Never lose the message: spool it so an admin can still read it out.
		m.spool(to, subject, msg)
		return err
	}
	return nil
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n    ")
}

func (m *Mailer) compose(to, subject, body string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", m.From)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	return []byte(b.String())
}

func (m *Mailer) spool(to, subject string, msg []byte) error {
	if m.Outbox == "" {
		return nil
	}
	name := fmt.Sprintf("%s-%s.eml", time.Now().Format("20060102-150405.000"), sanitizeFilename(to))
	return os.WriteFile(filepath.Join(m.Outbox, name), msg, 0o600)
}

func (m *Mailer) deliver(to string, msg []byte) error {
	addr := net.JoinHostPort(m.Host, fmt.Sprint(m.Port))
	c, err := smtp.Dial(addr)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Hello("localhost"); err != nil {
		return err
	}
	if m.StartTLS {
		if ok, _ := c.Extension("STARTTLS"); ok {
			cfg := &tls.Config{ServerName: m.Host, InsecureSkipVerify: m.Insecure}
			if err := c.StartTLS(cfg); err != nil {
				return err
			}
		}
	}
	if m.User != "" {
		auth := smtp.PlainAuth("", m.User, m.Pass, m.Host)
		if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail(m.From); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func sanitizeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
