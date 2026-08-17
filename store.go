package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ---------- domain types ----------

type Status string

const (
	StatusPending Status = "pending" // signed up, awaiting admin approval
	StatusActive  Status = "active"
	StatusFormer  Status = "former" // departed, or dropped for unpaid dues
	StatusSK      Status = "sk"     // silent key
	StatusBanned  Status = "banned"
)

var allStatuses = []Status{StatusPending, StatusActive, StatusFormer, StatusSK, StatusBanned}

func (s Status) Label() string {
	switch s {
	case StatusPending:
		return "Pending"
	case StatusActive:
		return "Active"
	case StatusFormer:
		return "Former"
	case StatusSK:
		return "Silent Key"
	case StatusBanned:
		return "Banned"
	}
	return string(s)
}

// Listable reports whether a status may ever appear in a directory listing.
func (s Status) Listable() bool { return s != StatusBanned && s != StatusPending }

type Role string

const (
	RoleMember Role = "member"
	RoleAdmin  Role = "admin"
)

type Address struct {
	Line1  string `json:"line1"`
	Line2  string `json:"line2"`
	City   string `json:"city"`
	State  string `json:"state"`
	Postal string `json:"postal"`
}

func (a Address) Empty() bool {
	return a.Line1 == "" && a.Line2 == "" && a.City == "" && a.State == "" && a.Postal == ""
}

func (a Address) OneLine() string {
	parts := []string{}
	for _, p := range []string{a.Line1, a.Line2} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	last := strings.TrimSpace(strings.TrimSpace(a.City+", "+a.State) + " " + a.Postal)
	last = strings.Trim(last, ", ")
	if last != "" {
		parts = append(parts, last)
	}
	return strings.Join(parts, " · ")
}

// Share holds the member's own consent choices. Only the member may change
// these — an admin can enter data but cannot publish it on someone's behalf.
type Share struct {
	Email   bool `json:"email"`
	Phone   bool `json:"phone"`
	Address bool `json:"address"`
	Photo   bool `json:"photo"`
}

type Member struct {
	ID       string `json:"id"`
	Email    string `json:"email"` // login identity, lowercased
	Name     string `json:"name"`
	CallSign string `json:"call_sign"`
	Role     Role   `json:"role"`

	Status          Status    `json:"status"`
	StatusReason    string    `json:"status_reason"`
	StatusChangedAt time.Time `json:"status_changed_at"`
	SKDate          string    `json:"sk_date,omitempty"`

	// Optional directory information.
	ContactEmail string  `json:"contact_email"`
	Phone        string  `json:"phone"`
	Address      Address `json:"address"`
	PhotoFile    string  `json:"photo_file"`
	Share        Share   `json:"share"`
	NeedsReview  bool    `json:"needs_review"`

	// Authentication.
	TOTPSecret   string     `json:"totp_secret,omitempty"`
	TOTPEnabled  *time.Time `json:"totp_enabled_at,omitempty"`
	TOTPLastStep int64      `json:"totp_last_step,omitempty"`
	BackupCodes  []string   `json:"backup_codes,omitempty"` // sha256 hex, single use
	SessionEpoch int        `json:"session_epoch"`

	AdminNotes  string     `json:"admin_notes"`
	JoinedAt    time.Time  `json:"joined_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

func (m *Member) IsAdmin() bool { return m.Role == RoleAdmin }
func (m *Member) MFAOn() bool   { return m.TOTPEnabled != nil }
func (m *Member) Display() string {
	if m.Name != "" {
		return m.Name
	}
	if m.CallSign != "" {
		return m.CallSign
	}
	return m.Email
}

// Sortkey groups by call sign, falling back to name.
func (m *Member) SortKey() string {
	if m.CallSign != "" {
		return strings.ToUpper(m.CallSign)
	}
	return "ZZZZ" + strings.ToUpper(m.Name)
}

type Settings struct {
	ClubName        string `json:"club_name"`
	ClubCallSign    string `json:"club_call_sign"`
	Tagline         string `json:"tagline"`
	PublicDirectory bool   `json:"public_directory"` // allow reading name+call without login
	SignupsOpen     bool   `json:"signups_open"`
	ShowSKPublicly  bool   `json:"show_sk_publicly"`
	ShowFormerPub   bool   `json:"show_former_publicly"`
}

// ---------- store ----------
//
// Structured data (roster, settings, the cookie signing key) lives in a
// SQLite database at <dir>/directory.db. Photos and spooled mail stay as
// plain files on disk, as before — only the record store moved.

type Store struct {
	db  *sql.DB
	dir string
}

var ErrNotFound = errors.New("not found")
var ErrDuplicate = errors.New("email already on the roster")

const schema = `
CREATE TABLE IF NOT EXISTS meta (
	id                INTEGER PRIMARY KEY CHECK (id = 1),
	version           INTEGER NOT NULL,
	secret            TEXT NOT NULL,
	club_name         TEXT NOT NULL,
	club_call_sign    TEXT NOT NULL,
	tagline           TEXT NOT NULL,
	public_directory  INTEGER NOT NULL,
	signups_open      INTEGER NOT NULL,
	show_sk_publicly  INTEGER NOT NULL,
	show_former_pub   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS members (
	id                 TEXT PRIMARY KEY,
	email              TEXT NOT NULL UNIQUE,
	name               TEXT NOT NULL DEFAULT '',
	call_sign          TEXT NOT NULL DEFAULT '',
	role               TEXT NOT NULL,
	status             TEXT NOT NULL,
	status_reason      TEXT NOT NULL DEFAULT '',
	status_changed_at  TEXT NOT NULL,
	sk_date            TEXT NOT NULL DEFAULT '',
	contact_email      TEXT NOT NULL DEFAULT '',
	phone              TEXT NOT NULL DEFAULT '',
	address_line1      TEXT NOT NULL DEFAULT '',
	address_line2      TEXT NOT NULL DEFAULT '',
	address_city       TEXT NOT NULL DEFAULT '',
	address_state      TEXT NOT NULL DEFAULT '',
	address_postal     TEXT NOT NULL DEFAULT '',
	photo_file         TEXT NOT NULL DEFAULT '',
	share_email        INTEGER NOT NULL DEFAULT 0,
	share_phone        INTEGER NOT NULL DEFAULT 0,
	share_address      INTEGER NOT NULL DEFAULT 0,
	share_photo        INTEGER NOT NULL DEFAULT 0,
	needs_review       INTEGER NOT NULL DEFAULT 0,
	totp_secret        TEXT NOT NULL DEFAULT '',
	totp_enabled_at    TEXT,
	totp_last_step     INTEGER NOT NULL DEFAULT 0,
	backup_codes       TEXT NOT NULL DEFAULT '[]',
	session_epoch      INTEGER NOT NULL DEFAULT 0,
	admin_notes        TEXT NOT NULL DEFAULT '',
	joined_at          TEXT NOT NULL,
	updated_at         TEXT NOT NULL,
	last_login_at      TEXT
);
`

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "photos"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "outbox"), 0o700); err != nil {
		return nil, err
	}
	dsn := filepath.Join(dir, "directory.db") + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", dsn, err)
	}
	// The roster is a few hundred rows at most; one connection avoids
	// SQLITE_BUSY entirely and gives every Store method the same
	// read-your-writes consistency the old single-mutex JSON store had.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enabling WAL: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating schema: %w", err)
	}
	s := &Store{db: db, dir: dir}
	if err := s.ensureMeta(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) ensureMeta() error {
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM meta WHERE id = 1").Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	set := Settings{
		ClubName:        "Club Directory",
		Tagline:         "Members and call signs",
		PublicDirectory: true,
		SignupsOpen:     false,
		ShowSKPublicly:  true,
	}
	_, err := s.db.Exec(`INSERT INTO meta
		(id, version, secret, club_name, club_call_sign, tagline, public_directory, signups_open, show_sk_publicly, show_former_pub)
		VALUES (1, 1, ?, ?, ?, ?, ?, ?, ?, ?)`,
		base64.StdEncoding.EncodeToString(key),
		set.ClubName, set.ClubCallSign, set.Tagline,
		boolToInt(set.PublicDirectory), boolToInt(set.SignupsOpen),
		boolToInt(set.ShowSKPublicly), boolToInt(set.ShowFormerPub))
	return err
}

func (s *Store) Secret() []byte {
	var secret string
	if err := s.db.QueryRow("SELECT secret FROM meta WHERE id = 1").Scan(&secret); err != nil {
		return nil
	}
	b, _ := base64.StdEncoding.DecodeString(secret)
	return b
}

func (s *Store) Settings() Settings {
	var set Settings
	var pub, signups, sk, former int
	err := s.db.QueryRow(`SELECT club_name, club_call_sign, tagline, public_directory, signups_open, show_sk_publicly, show_former_pub
		FROM meta WHERE id = 1`).Scan(&set.ClubName, &set.ClubCallSign, &set.Tagline, &pub, &signups, &sk, &former)
	if err != nil {
		return Settings{}
	}
	set.PublicDirectory = pub != 0
	set.SignupsOpen = signups != 0
	set.ShowSKPublicly = sk != 0
	set.ShowFormerPub = former != 0
	return set
}

func (s *Store) SaveSettings(set Settings) error {
	_, err := s.db.Exec(`UPDATE meta SET club_name = ?, club_call_sign = ?, tagline = ?,
		public_directory = ?, signups_open = ?, show_sk_publicly = ?, show_former_pub = ? WHERE id = 1`,
		set.ClubName, set.ClubCallSign, set.Tagline,
		boolToInt(set.PublicDirectory), boolToInt(set.SignupsOpen),
		boolToInt(set.ShowSKPublicly), boolToInt(set.ShowFormerPub))
	return err
}

func (s *Store) PhotoDir() string  { return filepath.Join(s.dir, "photos") }
func (s *Store) OutboxDir() string { return filepath.Join(s.dir, "outbox") }
func (s *Store) Dir() string       { return s.dir }

// ---------- row <-> Member mapping ----------

type scanner interface {
	Scan(dest ...any) error
}

func scanMember(row scanner) (*Member, error) {
	var m Member
	var statusChangedAt, joinedAt, updatedAt string
	var totpEnabledAt, lastLoginAt sql.NullString
	var backupCodes string
	var shareEmail, sharePhone, shareAddress, sharePhoto, needsReview int

	err := row.Scan(
		&m.ID, &m.Email, &m.Name, &m.CallSign, &m.Role,
		&m.Status, &m.StatusReason, &statusChangedAt, &m.SKDate,
		&m.ContactEmail, &m.Phone,
		&m.Address.Line1, &m.Address.Line2, &m.Address.City, &m.Address.State, &m.Address.Postal,
		&m.PhotoFile,
		&shareEmail, &sharePhone, &shareAddress, &sharePhoto, &needsReview,
		&m.TOTPSecret, &totpEnabledAt, &m.TOTPLastStep, &backupCodes, &m.SessionEpoch,
		&m.AdminNotes, &joinedAt, &updatedAt, &lastLoginAt,
	)
	if err != nil {
		return nil, err
	}
	m.StatusChangedAt = parseTime(statusChangedAt)
	m.JoinedAt = parseTime(joinedAt)
	m.UpdatedAt = parseTime(updatedAt)
	if totpEnabledAt.Valid {
		t := parseTime(totpEnabledAt.String)
		m.TOTPEnabled = &t
	}
	if lastLoginAt.Valid {
		t := parseTime(lastLoginAt.String)
		m.LastLoginAt = &t
	}
	m.Share = Share{Email: shareEmail != 0, Phone: sharePhone != 0, Address: shareAddress != 0, Photo: sharePhoto != 0}
	m.NeedsReview = needsReview != 0
	if backupCodes != "" {
		_ = json.Unmarshal([]byte(backupCodes), &m.BackupCodes)
	}
	return &m, nil
}

// memberCols is the single source of truth for member column order: SELECT,
// INSERT and UPDATE all derive their column lists from this slice, so it is
// impossible for scanMember, values() and the write statements to drift.
var memberCols = []string{
	"id", "email", "name", "call_sign", "role",
	"status", "status_reason", "status_changed_at", "sk_date",
	"contact_email", "phone",
	"address_line1", "address_line2", "address_city", "address_state", "address_postal",
	"photo_file",
	"share_email", "share_phone", "share_address", "share_photo", "needs_review",
	"totp_secret", "totp_enabled_at", "totp_last_step", "backup_codes", "session_epoch",
	"admin_notes", "joined_at", "updated_at", "last_login_at",
}

var memberColumns = strings.Join(memberCols, ", ")

// insertArgs and updateArgs share the same column order (minus id, which is
// fixed on insert and never changes on update).
func (m *Member) values() []any {
	codes, _ := json.Marshal(m.BackupCodes)
	return []any{
		m.Email, m.Name, m.CallSign, m.Role,
		m.Status, m.StatusReason, formatTime(m.StatusChangedAt), m.SKDate,
		m.ContactEmail, m.Phone,
		m.Address.Line1, m.Address.Line2, m.Address.City, m.Address.State, m.Address.Postal,
		m.PhotoFile,
		boolToInt(m.Share.Email), boolToInt(m.Share.Phone), boolToInt(m.Share.Address), boolToInt(m.Share.Photo),
		boolToInt(m.NeedsReview),
		m.TOTPSecret, nullableTime(m.TOTPEnabled), m.TOTPLastStep, string(codes), m.SessionEpoch,
		m.AdminNotes, formatTime(m.JoinedAt), formatTime(m.UpdatedAt), nullableTime(m.LastLoginAt),
	}
}

// clone returns a copy so callers can never mutate store state by accident.
func clone(m *Member) *Member {
	c := *m
	c.BackupCodes = append([]string(nil), m.BackupCodes...)
	return &c
}

func (s *Store) All() []*Member {
	rows, err := s.db.Query("SELECT " + memberColumns + " FROM members")
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []*Member{}
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SortKey() < out[j].SortKey() })
	return out
}

func (s *Store) ByID(id string) (*Member, error) {
	row := s.db.QueryRow("SELECT "+memberColumns+" FROM members WHERE id = ?", id)
	m, err := scanMember(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (s *Store) ByEmail(email string) (*Member, error) {
	row := s.db.QueryRow("SELECT "+memberColumns+" FROM members WHERE email = ?", normalizeEmail(email))
	m, err := scanMember(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (s *Store) Count() int {
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM members").Scan(&n)
	return n
}

func (s *Store) AdminCount() int {
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM members WHERE role = ? AND status = ?", RoleAdmin, StatusActive).Scan(&n)
	return n
}

func (s *Store) Create(m *Member) (*Member, error) {
	m.Email = normalizeEmail(m.Email)
	if m.ID == "" {
		m.ID = newID()
	}
	now := time.Now()
	if m.JoinedAt.IsZero() {
		m.JoinedAt = now
	}
	m.UpdatedAt = now
	m.StatusChangedAt = now

	args := append([]any{m.ID}, m.values()...)
	_, err := s.db.Exec(`INSERT INTO members (id, `+memberColumnsNoID()+`) VALUES (`+placeholders(len(args))+`)`, args...)
	if isUniqueViolation(err) {
		return nil, ErrDuplicate
	}
	if err != nil {
		return nil, err
	}
	return clone(m), nil
}

// Update fetches the live record, applies fn, then persists. SetMaxOpenConns(1)
// gives this the same read-modify-write atomicity the old global mutex did.
func (s *Store) Update(id string, fn func(*Member) error) (*Member, error) {
	m, err := s.ByID(id)
	if err != nil {
		return nil, err
	}
	before := *m
	if err := fn(m); err != nil {
		return nil, err
	}
	if m.Email != before.Email {
		m.Email = normalizeEmail(m.Email)
	}
	m.UpdatedAt = time.Now()

	args := append(m.values(), m.ID)
	_, err = s.db.Exec(`UPDATE members SET `+memberSetClause()+` WHERE id = ?`, args...)
	if isUniqueViolation(err) {
		return nil, ErrDuplicate
	}
	if err != nil {
		return nil, err
	}
	return clone(m), nil
}

func (s *Store) Delete(id string) error {
	m, err := s.ByID(id)
	if err != nil {
		return err
	}
	res, err := s.db.Exec("DELETE FROM members WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if m.PhotoFile != "" { // only after the record is really gone
		os.Remove(filepath.Join(s.dir, "photos", m.PhotoFile))
	}
	return nil
}

// ---------- visibility ----------

// Audience describes who is looking at a record.
type Audience int

const (
	AudPublic Audience = iota
	AudMember
	AudAdmin
)

// Access is what a signed-in record is allowed to do. It is deliberately the
// only place that decides this: audience, middleware and templates all read
// from here, so an unwrapped route cannot accidentally grant more than a
// wrapped one.
type Access int

const (
	AccessNone   Access = iota // no session, banned, or a silent key
	AccessSelf                 // may manage their own record and nothing else
	AccessMember               // may see what other members have shared
	AccessAdmin                // may see and manage everything
)

func accessOf(m *Member) Access {
	if m == nil {
		return AccessNone
	}
	switch m.Status {
	case StatusBanned, StatusSK:
		// A silent key's account should not be usable by anyone.
		return AccessNone
	case StatusPending:
		// Signed up but not approved: they can fill in their own record,
		// but the roster is not open to them yet.
		return AccessSelf
	case StatusFormer:
		// Former members keep self-service so they can withdraw details
		// they previously shared, but lose sight of the roster.
		return AccessSelf
	case StatusActive:
		if m.Role == RoleAdmin {
			if m.MFAOn() {
				return AccessAdmin
			}
			// Admin sight is earned by enrolling, not by holding the role.
			return AccessSelf
		}
		return AccessMember
	}
	return AccessNone
}

func audienceFor(viewer *Member) Audience {
	switch accessOf(viewer) {
	case AccessAdmin:
		return AudAdmin
	case AccessMember:
		return AudMember
	default:
		return AudPublic
	}
}

// Card is the sanitised projection of a Member for a given audience. Handlers
// and templates only ever see this, so a field that was not consented to
// cannot leak through a template mistake.
type Card struct {
	ID           string
	Name         string
	CallSign     string
	Status       Status
	StatusReason string
	SKDate       string
	Role         Role
	Email        string
	Phone        string
	Address      Address
	PhotoFile    string
	Self         bool
	// admin-only extras
	LoginEmail  string
	AdminNotes  string
	JoinedAt    time.Time
	LastLoginAt *time.Time
	MFAOn       bool
	NeedsReview bool
}

func (c Card) HasContact() bool {
	return c.Email != "" || c.Phone != "" || !c.Address.Empty()
}

// Visible reports whether this record should appear at all for the audience.
func visible(m *Member, viewer *Member, set Settings) bool {
	if viewer != nil && viewer.ID == m.ID {
		return true
	}
	if audienceFor(viewer) != AudAdmin && m.Name == "" && m.CallSign == "" {
		return false // nothing to list yet; don't advertise a half-filled record
	}
	switch audienceFor(viewer) {
	case AudAdmin:
		return true
	case AudMember:
		return m.Status.Listable()
	default:
		if !set.PublicDirectory || !m.Status.Listable() {
			return false
		}
		switch m.Status {
		case StatusActive:
			return true
		case StatusSK:
			return set.ShowSKPublicly
		case StatusFormer:
			return set.ShowFormerPub
		}
		return false
	}
}

func project(m *Member, viewer *Member) Card {
	self := viewer != nil && viewer.ID == m.ID
	aud := audienceFor(viewer)
	c := Card{
		ID:       m.ID,
		Name:     m.Name,
		CallSign: m.CallSign,
		Status:   m.Status,
		SKDate:   m.SKDate,
		Role:     m.Role,
		Self:     self,
	}
	// Name and call sign are the default share, and all anyone browsing
	// anonymously ever gets. Opted-in details go to signed-in members only;
	// the member themselves and admins see the whole record.
	full := aud == AudAdmin || self
	shared := full || aud == AudMember
	if full || (shared && m.Share.Email) {
		c.Email = m.ContactEmail
		if c.Email == "" {
			c.Email = m.Email
		}
	}
	if full || (shared && m.Share.Phone) {
		c.Phone = m.Phone
	}
	if full || (shared && m.Share.Address) {
		c.Address = m.Address
	}
	if full || (shared && m.Share.Photo) {
		c.PhotoFile = m.PhotoFile
	}
	if aud == AudAdmin || self {
		c.StatusReason = m.StatusReason
	}
	if aud == AudAdmin {
		c.LoginEmail = m.Email
		c.AdminNotes = m.AdminNotes
		c.JoinedAt = m.JoinedAt
		c.LastLoginAt = m.LastLoginAt
		c.MFAOn = m.MFAOn()
		c.NeedsReview = m.NeedsReview
	}
	return c
}

// Query filters the roster for a viewer.
type Query struct {
	Text   string
	Status string
	Role   string
}

func (s *Store) Search(viewer *Member, q Query) []Card {
	set := s.Settings()
	needle := strings.ToLower(strings.TrimSpace(q.Text))
	out := []Card{}
	for _, m := range s.All() {
		if !visible(m, viewer, set) {
			continue
		}
		if q.Status != "" && string(m.Status) != q.Status {
			continue
		}
		if q.Role != "" && string(m.Role) != q.Role {
			continue
		}
		c := project(m, viewer)
		if needle != "" && !cardMatches(c, needle) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func cardMatches(c Card, needle string) bool {
	hay := []string{c.Name, c.CallSign, c.Email, c.Phone, c.Address.City, c.Address.State, c.Address.Postal, c.LoginEmail}
	for _, h := range hay {
		if h != "" && strings.Contains(strings.ToLower(h), needle) {
			return true
		}
	}
	return false
}

// ---------- helpers ----------

func normalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func randToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// memberColumnsNoID, placeholders and memberSetClause keep the INSERT/UPDATE
// statements in the same column order as Member.values(), without repeating
// that order by hand in three places.
func memberColumnsNoID() string {
	return strings.Join(memberCols[1:], ", ") // drop "id"
}

func placeholders(n int) string {
	p := make([]string, n)
	for i := range p {
		p[i] = "?"
	}
	return strings.Join(p, ", ")
}

func memberSetClause() string {
	cols := memberCols[1:] // drop "id"
	set := make([]string, len(cols))
	for i, c := range cols {
		set[i] = c + " = ?"
	}
	return strings.Join(set, ", ")
}
