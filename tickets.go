package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

type TicketStatus string

const (
	TicketOpen           TicketStatus = "open"
	TicketWaitingSupport TicketStatus = "waiting_support" // submitter is waiting on a supporter
	TicketWaitingUser    TicketStatus = "waiting_user"    // supporter is waiting on the submitter
	TicketResolved       TicketStatus = "resolved"
)

var allTicketStatuses = []TicketStatus{TicketOpen, TicketWaitingSupport, TicketWaitingUser, TicketResolved}

func (s TicketStatus) Label() string {
	switch s {
	case TicketOpen:
		return "Open"
	case TicketWaitingSupport:
		return "Waiting for support"
	case TicketWaitingUser:
		return "Waiting for you"
	case TicketResolved:
		return "Resolved"
	}
	return string(s)
}

func validTicketStatus(s TicketStatus) bool {
	for _, x := range allTicketStatuses {
		if x == s {
			return true
		}
	}
	return false
}

type Ticket struct {
	ID        string
	RoleID    string
	MemberID  string
	Subject   string
	Status    TicketStatus
	CreatedAt time.Time
	UpdatedAt time.Time
}

type TicketMessage struct {
	ID         string
	TicketID   string
	AuthorID   string
	AuthorName string
	Body       string
	CreatedAt  time.Time
}

const ticketColumns = `SELECT id, role_id, member_id, subject, status, created_at, updated_at`

func scanTicket(row scanner) (Ticket, error) {
	var t Ticket
	var createdAt, updatedAt string
	err := row.Scan(&t.ID, &t.RoleID, &t.MemberID, &t.Subject, &t.Status, &createdAt, &updatedAt)
	if err != nil {
		return Ticket{}, err
	}
	t.CreatedAt = parseTime(createdAt)
	t.UpdatedAt = parseTime(updatedAt)
	return t, nil
}

func (s *Store) TicketByID(id string) (Ticket, error) {
	row := s.db.QueryRow(ticketColumns+" FROM tickets WHERE id = ?", id)
	t, err := scanTicket(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Ticket{}, ErrNotFound
	}
	return t, err
}

func (s *Store) TicketsForMember(memberID string) []Ticket {
	return s.queryTickets(ticketColumns+" FROM tickets WHERE member_id = ? ORDER BY updated_at DESC", memberID)
}

func (s *Store) TicketsForRole(roleID string) []Ticket {
	return s.queryTickets(ticketColumns+" FROM tickets WHERE role_id = ? ORDER BY updated_at DESC", roleID)
}

func (s *Store) AllTickets() []Ticket {
	return s.queryTickets(ticketColumns + " FROM tickets ORDER BY updated_at DESC")
}

func (s *Store) queryTickets(q string, args ...any) []Ticket {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []Ticket{}
	for rows.Next() {
		t, err := scanTicket(rows)
		if err == nil {
			out = append(out, t)
		}
	}
	return out
}

// CreateTicket opens a ticket in the given role's queue with an initial
// message from the submitter.
const (
	maxTicketSubjectLen = 200
	maxTicketBodyLen    = 4000
)

func (s *Store) CreateTicket(roleID, memberID, subject, body string) (Ticket, error) {
	subject = strings.TrimSpace(subject)
	body = strings.TrimSpace(body)
	if subject == "" {
		return Ticket{}, fmt.Errorf("a ticket needs a subject")
	}
	if len(subject) > maxTicketSubjectLen {
		return Ticket{}, fmt.Errorf("subject is too long (%d characters, limit %d)", len(subject), maxTicketSubjectLen)
	}
	if body == "" {
		return Ticket{}, fmt.Errorf("describe the issue before submitting")
	}
	if len(body) > maxTicketBodyLen {
		return Ticket{}, fmt.Errorf("message is too long (%d characters, limit %d)", len(body), maxTicketBodyLen)
	}
	now := time.Now()
	t := Ticket{ID: newID(), RoleID: roleID, MemberID: memberID, Subject: subject, Status: TicketOpen,
		CreatedAt: now, UpdatedAt: now}
	// Resolved before opening the transaction: s.ByID uses s.db directly, and
	// with SetMaxOpenConns(1) calling it while tx holds the sole connection
	// would deadlock waiting for a connection only this same transaction has.
	authorName := ""
	if member, err := s.ByID(memberID); err == nil {
		authorName = member.Display()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Ticket{}, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO tickets (id, role_id, member_id, subject, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.RoleID, t.MemberID, t.Subject, t.Status, formatTime(t.CreatedAt), formatTime(t.UpdatedAt)); err != nil {
		return Ticket{}, err
	}
	if _, err := tx.Exec(`INSERT INTO ticket_messages (id, ticket_id, author_id, author_name, body, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, newID(), t.ID, memberID, authorName, strings.TrimSpace(body), formatTime(now)); err != nil {
		return Ticket{}, err
	}
	if err := tx.Commit(); err != nil {
		return Ticket{}, err
	}
	return t, nil
}

func (s *Store) MessagesForTicket(ticketID string) []TicketMessage {
	rows, err := s.db.Query(`SELECT id, ticket_id, author_id, author_name, body, created_at
		FROM ticket_messages WHERE ticket_id = ? ORDER BY created_at`, ticketID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []TicketMessage{}
	for rows.Next() {
		var m TicketMessage
		var createdAt string
		if rows.Scan(&m.ID, &m.TicketID, &m.AuthorID, &m.AuthorName, &m.Body, &createdAt) == nil {
			m.CreatedAt = parseTime(createdAt)
			out = append(out, m)
		}
	}
	return out
}

// AddTicketMessage appends a message and optionally changes status in one
// transaction, matching how the roster's admin_status form already combines
// a reason with a status change in a single POST.
func (s *Store) AddTicketMessage(ticketID, authorID, authorName, body string, newStatus TicketStatus) error {
	body = strings.TrimSpace(body)
	if len(body) > maxTicketBodyLen {
		return fmt.Errorf("message is too long (%d characters, limit %d)", len(body), maxTicketBodyLen)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now()
	if body != "" {
		if _, err := tx.Exec(`INSERT INTO ticket_messages (id, ticket_id, author_id, author_name, body, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`, newID(), ticketID, authorID, authorName, body, formatTime(now)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE tickets SET status = ?, updated_at = ? WHERE id = ?`,
		newStatus, formatTime(now), ticketID); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------- access ----------

// canAccessTicket reports whether me may view/reply to t: the submitter
// (always — needing help is legitimate even from a pending or former
// account), a currently-eligible supporter holding the ticket's role, or a
// full admin. A role assignment that has gone stale (the holder was
// suspended, banned, or left) does not carry queue access — see
// eligibleForRole.
func (a *App) canAccessTicket(me *Member, t Ticket) bool {
	if me == nil {
		return false
	}
	if accessOf(me) == AccessAdmin {
		return true
	}
	if me.ID == t.MemberID {
		return true
	}
	if !eligibleForRole(me) {
		return false
	}
	for _, r := range a.store.RolesFor(me.ID) {
		if r.ID == t.RoleID {
			return true
		}
	}
	return false
}

// ---------- email ----------

func (a *App) notifyTicketCreated(t Ticket, role CustomRole, submitter *Member) {
	supporters := a.store.MembersWithRole(role.ID)
	if len(supporters) == 0 {
		return
	}
	set := a.store.Settings()
	subject := fmt.Sprintf("[%s] New %s ticket: %s", set.ClubName, role.Name, t.Subject)
	url := fmt.Sprintf("%s/tickets/%s", a.cfg.BaseURL, t.ID)
	body := fmt.Sprintf(`Hello,

%s opened a new "%s" support ticket:

%s

View and reply: %s
`, submitter.Display(), role.Name, t.Subject, url)
	for _, sup := range supporters {
		if err := a.mailer.Send(sup.Email, subject, body); err != nil {
			log.Printf("ticket-created alert to %s: %v", sup.Email, err)
		}
	}
}

// notifyTicketUpdate emails everyone attached to the ticket (the submitter
// plus every supporter for its role) except whoever just acted — covering
// "multiple users can have a support role and they should all get [notified]".
func (a *App) notifyTicketUpdate(t Ticket, role CustomRole, actor *Member, message string, statusChanged bool) {
	submitter, err := a.store.ByID(t.MemberID)
	if err != nil {
		return
	}
	recipients := map[string]*Member{}
	if submitter.ID != actor.ID {
		recipients[submitter.ID] = submitter
	}
	for _, sup := range a.store.MembersWithRole(role.ID) {
		if sup.ID != actor.ID {
			recipients[sup.ID] = sup
		}
	}
	if len(recipients) == 0 {
		return
	}
	set := a.store.Settings()
	subject := fmt.Sprintf("[%s] Update on ticket: %s", set.ClubName, t.Subject)
	url := fmt.Sprintf("%s/tickets/%s", a.cfg.BaseURL, t.ID)
	what := "replied"
	if statusChanged {
		what = fmt.Sprintf("replied and set the status to %q", t.Status.Label())
	}
	body := fmt.Sprintf(`Hello,

%s %s on the "%s" ticket:

%s

View and reply: %s
`, actor.Display(), what, t.Subject, message, url)
	for _, r := range recipients {
		if err := a.mailer.Send(r.Email, subject, body); err != nil {
			log.Printf("ticket-update alert to %s: %v", r.Email, err)
		}
	}
}
