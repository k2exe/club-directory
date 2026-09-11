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
	deliverErr := m.deliver(to, msg)
	if deliverErr == nil {
		return nil
	}
	// SMTP failed, but the message is not lost as long as the fallback
	// spool succeeds — and a non-nil return here is exactly what tells a
	// caller like the NetRemind scheduler "not captured anywhere, keep
	// retrying." Returning the SMTP error unconditionally, spool success
	// or not, used to make every relay outage retry a durably-spooled
	// reminder every minute for the rest of its catch-up window: one
	// caller's claim never got marked sent, so nothing ever stopped it
	// trying again, and O_EXCL's very correctness (no more silent
	// overwrites) turned that into a pile of retained duplicate copies
	// instead of one. Only report failure — and only then — if spooling
	// the fallback copy ALSO fails, and say why on both counts rather than
	// letting one error mask the other.
	if spoolErr := m.spool(to, subject, msg); spoolErr != nil {
		return fmt.Errorf("relay failed (%w) and spooling the fallback copy also failed (%v)", deliverErr, spoolErr)
	}
	log.Printf("mail to %s: relay failed (%v), spooled to outbox instead", sanitizeHeaderValue(to), deliverErr)
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

// spoolNameAttempts bounds retries on an actual filename collision. A
// four-byte random suffix makes one vanishingly unlikely, but O_EXCL is
// what makes "unlikely" into "detected and handled" rather than "silently
// overwrites the previous message" if it ever does happen.
const spoolNameAttempts = 5

func (m *Mailer) spool(to, subject string, msg []byte) error {
	if m.Outbox == "" {
		return nil
	}
	var lastErr error
	for i := 0; i < spoolNameAttempts; i++ {
		name := fmt.Sprintf("%s-%s-%s.eml", time.Now().Format("20060102-150405.000"), sanitizeFilename(to), randToken(4))
		f, err := os.OpenFile(filepath.Join(m.Outbox, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if os.IsExist(err) {
				lastErr = err
				continue // collided — try again with a fresh random suffix
			}
			return err
		}
		_, werr := f.Write(msg)
		cerr := f.Close()
		if werr != nil {
			return werr
		}
		return cerr
	}
	return fmt.Errorf("spooling to outbox: %d consecutive filename collisions: %w", spoolNameAttempts, lastErr)
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
