package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type rosterData struct {
	Cards  []Card
	Query  Query
	Counts map[Status]int
	Total  int
}

func (a *App) handleAdminRoster(w http.ResponseWriter, r *http.Request) {
	q := Query{
		Text:   r.URL.Query().Get("q"),
		Status: r.URL.Query().Get("status"),
		Role:   r.URL.Query().Get("role"),
	}
	me := a.current(r)
	counts := map[Status]int{}
	for _, m := range a.store.All() {
		counts[m.Status]++
	}
	a.render(w, r, "admin_roster", "Roster", rosterData{
		Cards:  a.store.Search(me, q),
		Query:  q,
		Counts: counts,
		Total:  a.store.Count(),
	})
}

func (a *App) handleAdminNewForm(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "admin_new", "Add a member", nil)
}

func (a *App) handleAdminCreate(w http.ResponseWriter, r *http.Request) {
	me := a.current(r)
	email := normalizeEmail(r.FormValue("email"))
	if !strings.Contains(email, "@") {
		a.fail(w, r, "Enter a valid email address for the member.", "/admin/new")
		return
	}
	status := Status(r.FormValue("status"))
	if !validStatus(status) {
		status = StatusActive
	}
	role := Role(r.FormValue("role"))
	if role != RoleAdmin {
		role = RoleMember
	}
	m, err := a.store.Create(&Member{
		Email:    email,
		Name:     strings.TrimSpace(r.FormValue("name")),
		CallSign: strings.ToUpper(strings.TrimSpace(r.FormValue("call_sign"))),
		Role:     role,
		Status:   status,
	})
	if errors.Is(err, ErrDuplicate) {
		a.fail(w, r, "That email address is already on the roster.", "/admin/new")
		return
	}
	if err != nil {
		a.fail(w, r, "Could not add that member.", "/admin/new")
		return
	}
	a.audit.Write(me.Email, "admin.member_add", m.ID, m.Email, a.clientIP(r))
	if r.FormValue("send_invite") != "" {
		a.sendLink(m, "invite", "")
		a.ok(w, r, "Member added and an invitation link is on its way.", "/admin/member/"+m.ID)
		return
	}
	a.ok(w, r, "Member added.", "/admin/member/"+m.ID)
}

type editData struct {
	M         *Member
	IsSelf    bool
	LastAdmin bool
}

func (a *App) handleAdminEditForm(w http.ResponseWriter, r *http.Request) {
	m, err := a.store.ByID(r.PathValue("id"))
	if err != nil {
		a.notFound(w, r)
		return
	}
	me := a.current(r)
	a.render(w, r, "admin_edit", m.Display(), editData{
		M:         m,
		IsSelf:    me.ID == m.ID,
		LastAdmin: m.IsAdmin() && a.store.AdminCount() <= 1,
	})
}

func (a *App) handleAdminSave(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	me := a.current(r)
	target, err := a.store.ByID(id)
	if err != nil {
		a.notFound(w, r)
		return
	}
	newRole := Role(r.FormValue("role"))
	if newRole != RoleAdmin {
		newRole = RoleMember
	}
	if target.IsAdmin() && newRole != RoleAdmin && a.store.AdminCount() <= 1 {
		a.fail(w, r, "This is the only admin left. Promote someone else first.", "/admin/member/"+id)
		return
	}

	// An admin may record contact details, but recording is not publishing.
	// Changing a value withdraws the consent that covered the old value:
	// the member agreed to share *that* number, not whatever replaces it.
	touched := false
	_, err = a.store.Update(id, func(x *Member) error {
		x.Name = strings.TrimSpace(r.FormValue("name"))
		x.CallSign = strings.ToUpper(strings.TrimSpace(r.FormValue("call_sign")))
		newEmail := normalizeEmail(r.FormValue("email"))
		if newEmail != x.Email {
			x.SessionEpoch++ // an address change must not leave old sessions alive
		}
		x.Email = newEmail
		x.Role = newRole
		x.AdminNotes = strings.TrimSpace(r.FormValue("admin_notes"))

		if v := normalizeEmail(r.FormValue("contact_email")); v != x.ContactEmail {
			x.ContactEmail, x.Share.Email, touched = v, false, true
		}
		if v := strings.TrimSpace(r.FormValue("phone")); v != x.Phone {
			x.Phone, x.Share.Phone, touched = v, false, true
		}
		addr := Address{
			Line1:  strings.TrimSpace(r.FormValue("line1")),
			Line2:  strings.TrimSpace(r.FormValue("line2")),
			City:   strings.TrimSpace(r.FormValue("city")),
			State:  strings.TrimSpace(r.FormValue("state")),
			Postal: strings.TrimSpace(r.FormValue("postal")),
		}
		if addr != x.Address {
			x.Address, x.Share.Address, touched = addr, false, true
		}
		if touched {
			x.NeedsReview = true
		}
		return nil
	})
	if errors.Is(err, ErrDuplicate) {
		a.fail(w, r, "Another member already uses that email address.", "/admin/member/"+id)
		return
	}
	if err != nil {
		a.fail(w, r, "Could not save those changes.", "/admin/member/"+id)
		return
	}
	detail := ""
	if touched {
		detail = "contact details changed; awaiting member review"
	}
	a.audit.Write(me.Email, "admin.member_edit", id, detail, a.clientIP(r))
	msg := "Changes saved."
	if touched {
		msg = "Saved. The new contact details stay private until the member approves sharing them."
	}
	a.ok(w, r, msg, "/admin/member/"+id)
}

func (a *App) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	me := a.current(r)
	status := Status(r.FormValue("status"))
	if !validStatus(status) {
		a.fail(w, r, "Pick a valid status.", "/admin/member/"+id)
		return
	}
	target, err := a.store.ByID(id)
	if err != nil {
		a.notFound(w, r)
		return
	}
	if me.ID == id && status == StatusBanned {
		a.fail(w, r, "You cannot ban your own account.", "/admin/member/"+id)
		return
	}
	if target.IsAdmin() && status != StatusActive && a.store.AdminCount() <= 1 {
		a.fail(w, r, "This is the only active admin. Promote someone else first.", "/admin/member/"+id)
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	_, err = a.store.Update(id, func(x *Member) error {
		x.Status = status
		x.StatusReason = reason
		x.StatusChangedAt = time.Now()
		x.SKDate = strings.TrimSpace(r.FormValue("sk_date"))
		if status == StatusBanned {
			x.SessionEpoch++ // sign them out everywhere, immediately
		}
		return nil
	})
	if err != nil {
		a.fail(w, r, "Could not change that status.", "/admin/member/"+id)
		return
	}
	a.audit.Write(me.Email, "admin.status", id, string(status)+" "+reason, a.clientIP(r))
	a.ok(w, r, "Status updated to "+status.Label()+".", "/admin/member/"+id)
}

func (a *App) handleAdminInvite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	me := a.current(r)
	m, err := a.store.ByID(id)
	if err != nil {
		a.notFound(w, r)
		return
	}
	if m.Status == StatusBanned {
		a.fail(w, r, "Banned accounts cannot be sent sign-in links.", "/admin/member/"+id)
		return
	}
	a.sendLink(m, "invite", "")
	a.audit.Write(me.Email, "admin.invite", id, m.Email, a.clientIP(r))
	a.ok(w, r, "Sign-in link sent to "+m.Email+".", "/admin/member/"+id)
}

func (a *App) handleAdminDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	me := a.current(r)
	if me.ID == id {
		a.fail(w, r, "You cannot delete your own account.", "/admin/member/"+id)
		return
	}
	m, err := a.store.ByID(id)
	if err != nil {
		a.notFound(w, r)
		return
	}
	if m.IsAdmin() && a.store.AdminCount() <= 1 {
		a.fail(w, r, "This is the only admin left.", "/admin/member/"+id)
		return
	}
	if r.FormValue("confirm") != m.Email {
		a.fail(w, r, "Type the member's sign-in email exactly to confirm deletion.", "/admin/member/"+id)
		return
	}
	if err := a.store.Delete(id); err != nil {
		a.fail(w, r, "Could not delete that record.", "/admin/member/"+id)
		return
	}
	a.audit.Write(me.Email, "admin.member_delete", id, m.Email, a.clientIP(r))
	a.ok(w, r, "Deleted "+m.Email+" and their photo.", "/admin")
}

func (a *App) handleSettingsForm(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "admin_settings", "Settings", a.store.Settings())
}

func (a *App) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	me := a.current(r)
	on := func(k string) bool { return r.FormValue(k) != "" }
	set := Settings{
		ClubName:        strings.TrimSpace(r.FormValue("club_name")),
		ClubCallSign:    strings.ToUpper(strings.TrimSpace(r.FormValue("club_call_sign"))),
		Tagline:         strings.TrimSpace(r.FormValue("tagline")),
		PublicDirectory: on("public_directory"),
		SignupsOpen:     on("signups_open"),
		ShowSKPublicly:  on("show_sk"),
		ShowFormerPub:   on("show_former"),
	}
	if set.ClubName == "" {
		set.ClubName = "Club Directory"
	}
	if err := a.store.SaveSettings(set); err != nil {
		a.fail(w, r, "Could not save settings.", "/admin/settings")
		return
	}
	a.audit.Write(me.Email, "admin.settings", "", "", a.clientIP(r))
	a.ok(w, r, "Settings saved.", "/admin/settings")
}

func (a *App) handleAuditView(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "admin_log", "Activity log", a.audit.Tail(300))
}

func (a *App) handleExport(w http.ResponseWriter, r *http.Request) {
	me := a.current(r)
	a.audit.Write(me.Email, "admin.export", "", "csv", a.clientIP(r))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="roster-%s.csv"`, time.Now().Format("2006-01-02")))
	cw := csv.NewWriter(w)
	defer cw.Flush()
	cw.Write([]string{"call_sign", "name", "status", "status_reason", "role", "login_email",
		"contact_email", "phone", "address", "city", "state", "postal",
		"shares_email", "shares_phone", "shares_address", "shares_photo",
		"two_step", "joined", "last_login"})
	yn := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	for _, m := range a.store.All() {
		last := ""
		if m.LastLoginAt != nil {
			last = m.LastLoginAt.Format("2006-01-02")
		}
		addr := strings.TrimSpace(m.Address.Line1 + " " + m.Address.Line2)
		cw.Write([]string{csvSafe(m.CallSign), csvSafe(m.Name), string(m.Status), csvSafe(m.StatusReason),
			string(m.Role), csvSafe(m.Email), csvSafe(m.ContactEmail), csvSafe(m.Phone), csvSafe(addr),
			csvSafe(m.Address.City), csvSafe(m.Address.State), csvSafe(m.Address.Postal),
			yn(m.Share.Email), yn(m.Share.Phone), yn(m.Share.Address), yn(m.Share.Photo),
			yn(m.MFAOn()), m.JoinedAt.Format("2006-01-02"), last})
	}
}

// csvSafe stops a spreadsheet treating member-supplied text as a formula.
// A name like "=HYPERLINK(...)" is a valid thing to store and a dangerous
// thing to hand to Excel unescaped.
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

func validStatus(s Status) bool {
	for _, x := range allStatuses {
		if x == s {
			return true
		}
	}
	return false
}
