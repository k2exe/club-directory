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

	// DialTimeout/SMTPDeadline default to dialTimeout/smtpDeadline when
	// zero; tests set them short so a deliberately unreachable relay
	// doesn't cost real wall-clock seconds.
	DialTimeout  time.Duration
	SMTPDeadline time.Duration
}

func (m *Mailer) Configured() bool { return m.Host != "" }

// Send delivers a plain-text message. With no SMTP host configured it writes
// the message to the outbox directory instead, so a club can run the whole
// thing with no mail server at all and hand out links by other means.
func (m *Mailer) Send(to, subject, body string) error {
	msg := m.compose(to, subject, body)
	if m.Echo || !m.Configured() {
		// Sanitized for the same reason compose() sanitizes the header
		// values: to and subject are member-controlled, and an
		// unsanitized CR/LF here would forge fake log lines even though
		// it can no longer forge mail headers.
		log.Printf("mail to %s: %s\n%s", sanitizeHeaderValue(to), sanitizeHeaderValue(subject), indent(body))
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
	fmt.Fprintf(&b, "From: %s\r\n", sanitizeHeaderValue(m.From))
	fmt.Fprintf(&b, "To: %s\r\n", sanitizeHeaderValue(to))
	fmt.Fprintf(&b, "Subject: %s\r\n", sanitizeHeaderValue(subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	return []byte(b.String())
}

// sanitizeHeaderValue strips CR/LF (and other control characters) from a
// value bound for a raw RFC 5322 header line. Every header value here
// eventually comes from something a member typed — a ticket subject, a
// display name — and this is the one place all of them pass through before
// hitting the wire, so a "Subject: hi\r\nX-Injected: yes" can't inject
// extra headers or forge a From/To.
func sanitizeHeaderValue(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || (r < 0x20 && r != '\t') {
			return -1
		}
		return r
	}, s)
}

func (m *Mailer) spool(to, subject string, msg []byte) error {
	if m.Outbox == "" {
		return nil
	}
	// A millisecond timestamp plus recipient is not unique under concurrent
	// sends to the same address — two HTTP handlers spooling for the same
	// member in the same millisecond used to silently overwrite one
	// another via os.WriteFile. A short random suffix makes the name
	// unique regardless of clock resolution or how many land at once.
	name := fmt.Sprintf("%s-%s-%s.eml", time.Now().Format("20060102-150405.000"), sanitizeFilename(to), randToken(4))
	return os.WriteFile(filepath.Join(m.Outbox, name), msg, 0o600)
}

// dialTimeout bounds connecting to the relay; smtpDeadline bounds the whole
// conversation after that (handshake through data transfer) — smtp.Dial on
// its own has neither, so a relay that accepts the TCP connection and then
// never speaks (or a route that black-holes instead of refusing) hangs a
// reminder or ticket-notification send indefinitely.
const dialTimeout = 10 * time.Second
const smtpDeadline = 30 * time.Second

func (m *Mailer) deliver(to string, msg []byte) error {
	dt, sd := m.DialTimeout, m.SMTPDeadline
	if dt == 0 {
		dt = dialTimeout
	}
	if sd == 0 {
		sd = smtpDeadline
	}
	addr := net.JoinHostPort(m.Host, fmt.Sprint(m.Port))
	conn, err := net.DialTimeout("tcp", addr, dt)
	if err != nil {
		return err
	}
	if err := conn.SetDeadline(time.Now().Add(sd)); err != nil {
		conn.Close()
		return err
	}
	c, err := smtp.NewClient(conn, m.Host)
	if err != nil {
		conn.Close()
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
