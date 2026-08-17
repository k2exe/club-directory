package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
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

type data struct {
	Version  int       `json:"version"`
	Secret   string    `json:"secret"` // base64 HMAC key for cookies
	Settings Settings  `json:"settings"`
	Members  []*Member `json:"members"`
}

// ---------- store ----------

type Store struct {
	mu   sync.RWMutex
	path string
	dir  string
	d    *data
}

var ErrNotFound = errors.New("not found")
var ErrDuplicate = errors.New("email already on the roster")

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "photos"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "outbox"), 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "directory.json"), dir: dir}
	b, err := os.ReadFile(s.path)
	switch {
	case err == nil:
		s.d = &data{}
		if err := json.Unmarshal(b, s.d); err != nil {
			return nil, fmt.Errorf("reading %s: %w", s.path, err)
		}
	case os.IsNotExist(err):
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		s.d = &data{
			Version: 1,
			Secret:  base64.StdEncoding.EncodeToString(key),
			Settings: Settings{
				ClubName:        "Club Directory",
				Tagline:         "Members and call signs",
				PublicDirectory: true,
				SignupsOpen:     false,
				ShowSKPublicly:  true,
			},
		}
		if err := s.save(); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	return s, nil
}

// save assumes the caller holds the write lock (or is in construction).
func (s *Store) save() error {
	b, err := json.MarshalIndent(s.d, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) Secret() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, _ := base64.StdEncoding.DecodeString(s.d.Secret)
	return b
}

func (s *Store) Settings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.d.Settings
}

func (s *Store) SaveSettings(set Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.d.Settings
	s.d.Settings = set
	if err := s.save(); err != nil {
		s.d.Settings = prev
		return err
	}
	return nil
}

func (s *Store) PhotoDir() string  { return filepath.Join(s.dir, "photos") }
func (s *Store) OutboxDir() string { return filepath.Join(s.dir, "outbox") }
func (s *Store) Dir() string       { return s.dir }

// clone returns a copy so callers can never mutate store state by accident.
func clone(m *Member) *Member {
	c := *m
	c.BackupCodes = append([]string(nil), m.BackupCodes...)
	return &c
}

func (s *Store) All() []*Member {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Member, 0, len(s.d.Members))
	for _, m := range s.d.Members {
		out = append(out, clone(m))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SortKey() < out[j].SortKey() })
	return out
}

func (s *Store) ByID(id string) (*Member, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, m := range s.d.Members {
		if m.ID == id {
			return clone(m), nil
		}
	}
	return nil, ErrNotFound
}

func (s *Store) ByEmail(email string) (*Member, error) {
	email = normalizeEmail(email)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, m := range s.d.Members {
		if m.Email == email {
			return clone(m), nil
		}
	}
	return nil, ErrNotFound
}

func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.d.Members)
}

func (s *Store) AdminCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, m := range s.d.Members {
		if m.Role == RoleAdmin && m.Status == StatusActive {
			n++
		}
	}
	return n
}

func (s *Store) Create(m *Member) (*Member, error) {
	m.Email = normalizeEmail(m.Email)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.d.Members {
		if e.Email == m.Email {
			return nil, ErrDuplicate
		}
	}
	if m.ID == "" {
		m.ID = newID()
	}
	now := time.Now()
	if m.JoinedAt.IsZero() {
		m.JoinedAt = now
	}
	m.UpdatedAt = now
	m.StatusChangedAt = now
	s.d.Members = append(s.d.Members, m)
	if err := s.save(); err != nil {
		s.d.Members = s.d.Members[:len(s.d.Members)-1] // keep memory and disk in step
		return nil, err
	}
	return clone(m), nil
}

// Update applies fn to the live record under lock, then persists.
func (s *Store) Update(id string, fn func(*Member) error) (*Member, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.d.Members {
		if m.ID != id {
			continue
		}
		before := *m
		if err := fn(m); err != nil {
			*m = before
			return nil, err
		}
		if m.Email != before.Email {
			m.Email = normalizeEmail(m.Email)
			for _, o := range s.d.Members {
				if o.ID != id && o.Email == m.Email {
					*m = before
					return nil, ErrDuplicate
				}
			}
		}
		m.UpdatedAt = time.Now()
		if err := s.save(); err != nil {
			*m = before
			return nil, err
		}
		return clone(m), nil
	}
	return nil, ErrNotFound
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, m := range s.d.Members {
		if m.ID == id {
			kept := append([]*Member(nil), s.d.Members...)
			s.d.Members = append(s.d.Members[:i:i], s.d.Members[i+1:]...)
			if err := s.save(); err != nil {
				s.d.Members = kept
				return err
			}
			if m.PhotoFile != "" { // only after the record is really gone
				os.Remove(filepath.Join(s.dir, "photos", m.PhotoFile))
			}
			return nil
		}
	}
	return ErrNotFound
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
