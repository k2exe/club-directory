package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// CustomRole is a club-defined tag such as "Web Admin" or "Repeater Admin".
// Holding one grants a ticket queue in that name (see tickets.go), and
// optionally two capabilities: net scheduling, and lifecycle email alerts.
type CustomRole struct {
	ID             string
	Name           string
	Color          string // "#rrggbb"
	IsLifecycle    bool
	GrantsNetAdmin bool
	CreatedAt      time.Time
}

// ColorPreset is one of the clickable swatches offered in the role editor.
// Deliberately distinct from the roster's status-dot colors (green/amber/
// signal-red already mean Active/Pending/danger elsewhere in this app).
type ColorPreset struct {
	Name  string
	Hex   string
	Class string // CSS class in app.css, e.g. "swatch-blue"
}

var colorPresets = []ColorPreset{
	{"Blue", "#2f6690", "swatch-blue"},
	{"Purple", "#7d5ba6", "swatch-purple"},
	{"Teal", "#2f8f8f", "swatch-teal"},
	{"Orange", "#c8752b", "swatch-orange"},
	{"Pink", "#b5476b", "swatch-pink"},
	{"Indigo", "#4a5a9e", "swatch-indigo"},
	{"Brown", "#7a5c3e", "swatch-brown"},
	{"Slate", "#55606b", "swatch-slate"},
}

const defaultRoleColor = "#55606b" // slate

var hexColorRE = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// normalizeColor accepts a preset pick or a custom hex value (from a native
// <input type="color">, which always sends a well-formed value) and falls
// back to slate if neither is valid.
func normalizeColor(preset, custom string) string {
	for _, p := range colorPresets {
		if preset == p.Hex {
			return p.Hex
		}
	}
	if hexColorRE.MatchString(custom) {
		return strings.ToLower(custom)
	}
	return defaultRoleColor
}

func (s *Store) CustomRoles() []CustomRole {
	rows, err := s.db.Query(`SELECT id, name, color, is_lifecycle, grants_net_admin, created_at
		FROM custom_roles ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []CustomRole{}
	for rows.Next() {
		r, err := scanCustomRole(rows)
		if err == nil {
			out = append(out, r)
		}
	}
	return out
}

func scanCustomRole(row scanner) (CustomRole, error) {
	var r CustomRole
	var lifecycle, netAdmin int
	var createdAt string
	err := row.Scan(&r.ID, &r.Name, &r.Color, &lifecycle, &netAdmin, &createdAt)
	if err != nil {
		return CustomRole{}, err
	}
	r.IsLifecycle = lifecycle != 0
	r.GrantsNetAdmin = netAdmin != 0
	r.CreatedAt = parseTime(createdAt)
	return r, nil
}

func (s *Store) CustomRoleByID(id string) (CustomRole, error) {
	row := s.db.QueryRow(`SELECT id, name, color, is_lifecycle, grants_net_admin, created_at
		FROM custom_roles WHERE id = ?`, id)
	r, err := scanCustomRole(row)
	if errors.Is(err, sql.ErrNoRows) {
		return CustomRole{}, ErrNotFound
	}
	return r, err
}

// SaveCustomRole creates a role (empty ID) or updates one in place.
func (s *Store) SaveCustomRole(r CustomRole) (CustomRole, error) {
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		return CustomRole{}, fmt.Errorf("a role needs a name")
	}
	if r.Color == "" {
		r.Color = defaultRoleColor
	}
	if r.ID == "" {
		r.ID = newID()
		r.CreatedAt = time.Now()
		_, err := s.db.Exec(`INSERT INTO custom_roles (id, name, color, is_lifecycle, grants_net_admin, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			r.ID, r.Name, r.Color, boolToInt(r.IsLifecycle), boolToInt(r.GrantsNetAdmin), formatTime(r.CreatedAt))
		if isUniqueViolation(err) {
			return CustomRole{}, fmt.Errorf("a role named %q already exists", r.Name)
		}
		return r, err
	}
	_, err := s.db.Exec(`UPDATE custom_roles SET name = ?, color = ?, is_lifecycle = ?, grants_net_admin = ? WHERE id = ?`,
		r.Name, r.Color, boolToInt(r.IsLifecycle), boolToInt(r.GrantsNetAdmin), r.ID)
	if isUniqueViolation(err) {
		return CustomRole{}, fmt.Errorf("a role named %q already exists", r.Name)
	}
	return r, err
}

func (s *Store) DeleteCustomRole(id string) error {
	res, err := s.db.Exec("DELETE FROM custom_roles WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RolesFor returns the custom roles a member holds.
func (s *Store) RolesFor(memberID string) []CustomRole {
	rows, err := s.db.Query(`SELECT cr.id, cr.name, cr.color, cr.is_lifecycle, cr.grants_net_admin, cr.created_at
		FROM custom_roles cr JOIN member_roles mr ON mr.role_id = cr.id
		WHERE mr.member_id = ? ORDER BY cr.name COLLATE NOCASE`, memberID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []CustomRole{}
	for rows.Next() {
		r, err := scanCustomRole(rows)
		if err == nil {
			out = append(out, r)
		}
	}
	return out
}

// MembersWithRole returns everyone holding a given custom role — the
// supporters for that role's ticket queue, and (if the role is lifecycle)
// the recipients of roster-change alerts.
func (s *Store) MembersWithRole(roleID string) []*Member {
	rows, err := s.db.Query(`SELECT member_id FROM member_roles WHERE role_id = ?`, roleID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	out := []*Member{}
	for _, id := range ids {
		if m, err := s.ByID(id); err == nil {
			out = append(out, m)
		}
	}
	return out
}

func (s *Store) SetMemberRoles(memberID string, roleIDs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM member_roles WHERE member_id = ?", memberID); err != nil {
		return err
	}
	for _, rid := range roleIDs {
		if rid == "" {
			continue
		}
		if _, err := tx.Exec("INSERT OR IGNORE INTO member_roles (member_id, role_id) VALUES (?, ?)", memberID, rid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GrantsNetAdmin reports whether a member holds any role that grants net
// scheduling. Full Admins already bypass this check at the call site.
func (s *Store) GrantsNetAdmin(memberID string) bool {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM member_roles mr JOIN custom_roles cr ON cr.id = mr.role_id
		WHERE mr.member_id = ? AND cr.grants_net_admin = 1`, memberID).Scan(&n)
	return n > 0
}

// lifecycleRecipients returns every member holding a lifecycle-flagged role.
func (s *Store) lifecycleRecipients() []*Member {
	rows, err := s.db.Query(`SELECT DISTINCT mr.member_id FROM member_roles mr
		JOIN custom_roles cr ON cr.id = mr.role_id WHERE cr.is_lifecycle = 1`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	out := []*Member{}
	for _, id := range ids {
		if m, err := s.ByID(id); err == nil {
			out = append(out, m)
		}
	}
	return out
}

// handleRolesCSS serves one `--role-color` custom property per custom role,
// so a role's exact color (preset or a fully custom pick from the native
// color wheel) can be used by class name (`role-<id>`) anywhere in the UI —
// without inline `style=`, which the app's CSP (`style-src 'self'`) forbids.
func (a *App) handleRolesCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	for _, role := range a.store.CustomRoles() {
		fmt.Fprintf(w, ".role-%s { --role-color: %s; }\n", role.ID, role.Color)
	}
}

// notifyLifecycle emails everyone subscribed to roster lifecycle events.
// event is a short human label ("added to the roster", "status changed to
// Banned") describing what just happened to subject.
func (a *App) notifyLifecycle(event string, subject *Member, detail string) {
	recipients := a.store.lifecycleRecipients()
	if len(recipients) == 0 {
		return
	}
	set := a.store.Settings()
	who := subject.Display()
	subjectLine := fmt.Sprintf("[%s roster] %s %s", set.ClubName, who, event)
	body := fmt.Sprintf(`Hello,

This is a lifecycle alert from %s.

Member: %s
Event: %s
`, set.ClubName, who, event)
	if detail != "" {
		body += fmt.Sprintf("Detail: %s\n", detail)
	}
	body += fmt.Sprintf("\nSign in to review: %s/admin/member/%s\n", a.cfg.BaseURL, subject.ID)
	for _, r := range recipients {
		if err := a.mailer.Send(r.Email, subjectLine, body); err != nil {
			log.Printf("lifecycle alert to %s: %v", r.Email, err)
		}
	}
}
