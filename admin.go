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
	a.notifyLifecycle("was added to the roster", m, "added by "+me.Display())
	if r.FormValue("send_invite") != "" {
		a.sendLink(m, "invite", "")
		a.ok(w, r, "Member added and an invitation link is on its way.", "/admin/member/"+m.ID)
		return
	}
	a.ok(w, r, "Member added.", "/admin/member/"+m.ID)
}

type editData struct {
	M           *Member
	IsSelf      bool
	LastAdmin   bool
	AllRoles    []CustomRole
	AssignedIDs map[string]bool
}

func (a *App) handleAdminEditForm(w http.ResponseWriter, r *http.Request) {
	m, err := a.store.ByID(r.PathValue("id"))
	if err != nil {
		a.notFound(w, r)
		return
	}
	me := a.current(r)
	assigned := map[string]bool{}
	for _, ro := range a.store.RolesFor(m.ID) {
		assigned[ro.ID] = true
	}
	a.render(w, r, "admin_edit", m.Display(), editData{
		M:           m,
		IsSelf:      me.ID == m.ID,
		LastAdmin:   m.IsAdmin() && a.store.AdminCount() <= 1,
		AllRoles:    a.store.CustomRoles(),
		AssignedIDs: assigned,
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
	if status != target.Status {
		detail := "changed by " + me.Display()
		if reason != "" {
			detail += ": " + reason
		}
		if updated, err := a.store.ByID(id); err == nil {
			a.notifyLifecycle(fmt.Sprintf("status changed to %s", status.Label()), updated, detail)
		}
	}
	a.ok(w, r, "Status updated to "+status.Label()+".", "/admin/member/"+id)
}

// ---------- custom roles & assignment ----------

func (a *App) handleAdminRoleAssign(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	me := a.current(r)
	if _, err := a.store.ByID(id); err != nil {
		a.notFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		a.fail(w, r, "Could not read that form.", "/admin/member/"+id)
		return
	}
	if err := a.store.SetMemberRoles(id, r.Form["roles"]); err != nil {
		a.fail(w, r, "Could not save role assignments.", "/admin/member/"+id)
		return
	}
	a.audit.Write(me.Email, "admin.roles_assigned", id, strings.Join(r.Form["roles"], ","), a.clientIP(r))
	a.ok(w, r, "Roles updated.", "/admin/member/"+id)
}

type rolesData struct {
	Roles   []CustomRole
	Presets []ColorPreset
	Editing *CustomRole
}

func (a *App) handleAdminRoles(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "admin_roles", "Custom roles", rolesData{Roles: a.store.CustomRoles(), Presets: colorPresets})
}

func (a *App) handleAdminRoleEditForm(w http.ResponseWriter, r *http.Request) {
	role, err := a.store.CustomRoleByID(r.PathValue("id"))
	if err != nil {
		a.notFound(w, r)
		return
	}
	a.render(w, r, "admin_roles", "Edit "+role.Name, rolesData{
		Roles: a.store.CustomRoles(), Presets: colorPresets, Editing: &role,
	})
}

func (a *App) handleAdminRoleCreate(w http.ResponseWriter, r *http.Request) {
	me := a.current(r)
	role := CustomRole{
		Name:           r.FormValue("name"),
		Color:          normalizeColor(r.FormValue("color_preset"), r.FormValue("color_custom")),
		IsLifecycle:    r.FormValue("is_lifecycle") != "",
		GrantsNetAdmin: r.FormValue("grants_net_admin") != "",
	}
	saved, err := a.store.SaveCustomRole(role)
	if err != nil {
		a.fail(w, r, err.Error(), "/admin/roles")
		return
	}
	a.audit.Write(me.Email, "admin.role_create", saved.ID, saved.Name, a.clientIP(r))
	a.ok(w, r, "Role created.", "/admin/roles")
}

func (a *App) handleAdminRoleSave(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	me := a.current(r)
	existing, err := a.store.CustomRoleByID(id)
	if err != nil {
		a.notFound(w, r)
		return
	}
	existing.Name = r.FormValue("name")
	existing.Color = normalizeColor(r.FormValue("color_preset"), r.FormValue("color_custom"))
	existing.IsLifecycle = r.FormValue("is_lifecycle") != ""
	existing.GrantsNetAdmin = r.FormValue("grants_net_admin") != ""
	saved, err := a.store.SaveCustomRole(existing)
	if err != nil {
		a.fail(w, r, err.Error(), "/admin/roles/"+id)
		return
	}
	a.audit.Write(me.Email, "admin.role_edit", saved.ID, saved.Name, a.clientIP(r))
	a.ok(w, r, "Role saved.", "/admin/roles")
}

func (a *App) handleAdminRoleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	me := a.current(r)
	role, err := a.store.CustomRoleByID(id)
	if err != nil {
		a.notFound(w, r)
		return
	}
	if err := a.store.DeleteCustomRole(id); err != nil {
		if errors.Is(err, ErrRoleHasTickets) {
			a.fail(w, r, "This role still has tickets in its queue. Resolve or reassign them before deleting the role.", "/admin/roles")
			return
		}
		a.fail(w, r, "Could not delete that role.", "/admin/roles")
		return
	}
	a.audit.Write(me.Email, "admin.role_delete", id, role.Name, a.clientIP(r))
	a.ok(w, r, "Role deleted.", "/admin/roles")
}

// ---------- repeaters ----------

type repeatersData struct {
	Repeaters []Repeater
	Editing   *Repeater
}

func (a *App) handleAdminRepeaters(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "admin_repeaters", "Repeaters", repeatersData{Repeaters: a.store.Repeaters()})
}

func (a *App) handleAdminRepeaterCreate(w http.ResponseWriter, r *http.Request) {
	me := a.current(r)
	rp := Repeater{Name: r.FormValue("name"), Frequency: strings.TrimSpace(r.FormValue("frequency")),
		Notes: strings.TrimSpace(r.FormValue("notes"))}
	saved, err := a.store.SaveRepeater(rp)
	if err != nil {
		a.fail(w, r, err.Error(), "/admin/repeaters")
		return
	}
	a.audit.Write(me.Email, "admin.repeater_create", saved.ID, saved.Name, a.clientIP(r))
	a.ok(w, r, "Repeater added.", "/admin/repeaters")
}

func (a *App) handleAdminRepeaterDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	me := a.current(r)
	rp, err := a.store.RepeaterByID(id)
	if err != nil {
		a.notFound(w, r)
		return
	}
	if err := a.store.DeleteRepeater(id); err != nil {
		a.fail(w, r, "Could not delete that repeater.", "/admin/repeaters")
		return
	}
	a.audit.Write(me.Email, "admin.repeater_delete", id, rp.Name, a.clientIP(r))
	a.ok(w, r, "Repeater deleted.", "/admin/repeaters")
}

// ---------- nets (net admin: full admins, or a role with GrantsNetAdmin) ----------

type netFormData struct {
	Net       *Net
	Repeaters []Repeater
	Kinds     []RecurKind
	Weekdays  []time.Weekday
}

func (a *App) handleNetNewForm(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "net_form", "Schedule a net", netFormData{
		Repeaters: a.store.Repeaters(), Kinds: allRecurKinds, Weekdays: allWeekdays,
	})
}

func (a *App) handleNetEditForm(w http.ResponseWriter, r *http.Request) {
	n, err := a.store.NetByID(r.PathValue("id"))
	if err != nil {
		a.notFound(w, r)
		return
	}
	a.render(w, r, "net_form", "Edit "+n.Name, netFormData{
		Net: &n, Repeaters: a.store.Repeaters(), Kinds: allRecurKinds, Weekdays: allWeekdays,
	})
}

func validRecurKind(k RecurKind) bool {
	for _, x := range allRecurKinds {
		if x == k {
			return true
		}
	}
	return false
}

func netFromForm(r *http.Request) (Net, error) {
	loc := time.UTC
	starts, err := time.ParseInLocation("2006-01-02T15:04", r.FormValue("starts_at"), loc)
	if err != nil {
		return Net{}, fmt.Errorf("enter a valid start date and time")
	}
	recur := RecurKind(r.FormValue("recur_kind"))
	if !validRecurKind(recur) {
		return Net{}, fmt.Errorf("pick a valid recurrence")
	}
	n := Net{
		Name:       strings.TrimSpace(r.FormValue("name")),
		Frequency:  strings.TrimSpace(r.FormValue("frequency")),
		RepeaterID: r.FormValue("repeater_id"),
		Mode:       strings.TrimSpace(r.FormValue("mode")),
		StartsAt:   starts,
		Recur:      recur,
	}
	for _, d := range allWeekdays {
		if r.FormValue(fmt.Sprintf("weekday_%d", int(d))) != "" {
			n.WeekdaysMask |= weekdayBit(d)
		}
	}
	switch r.FormValue("weekly_preset") {
	case "weekdays":
		n.WeekdaysMask = weekdaysMask
	case "weekends":
		n.WeekdaysMask = weekendsMask
	}
	if n.Recur == RecurMonthlyNth {
		var week, wd int
		if _, err := fmt.Sscanf(r.FormValue("nth_week"), "%d", &week); err != nil || week < 1 || week > 5 {
			return Net{}, fmt.Errorf("pick a week (1st through last) for a monthly-Nth-weekday net")
		}
		if _, err := fmt.Sscanf(r.FormValue("nth_weekday"), "%d", &wd); err != nil || wd < 0 || wd > 6 {
			return Net{}, fmt.Errorf("pick a valid weekday for a monthly-Nth-weekday net")
		}
		n.NthWeek, n.NthWeekday = week, time.Weekday(wd)
	}
	if end := strings.TrimSpace(r.FormValue("ends_at")); end != "" {
		d, err := time.ParseInLocation("2006-01-02", end, loc)
		if err != nil {
			return Net{}, fmt.Errorf("enter a valid end date, or leave it blank")
		}
		// Inclusive of the whole day: "stop repeating after Sept 10" should
		// still run a net at, say, 19:00 on the 10th — not exclude it by
		// comparing against midnight.
		endOfDay := time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 0, loc)
		if endOfDay.Before(n.StartsAt) {
			return Net{}, fmt.Errorf("the end date can't be before the first occurrence")
		}
		n.EndsAt = &endOfDay
	}
	return n, nil
}

func (a *App) handleNetCreate(w http.ResponseWriter, r *http.Request) {
	me := a.current(r)
	n, err := netFromForm(r)
	if err != nil {
		a.fail(w, r, err.Error(), "/nets/new")
		return
	}
	n.CreatedBy = me.ID
	saved, err := a.store.SaveNet(n)
	if err != nil {
		a.fail(w, r, err.Error(), "/nets/new")
		return
	}
	a.audit.Write(me.Email, "net.create", saved.ID, saved.Name, a.clientIP(r))
	a.ok(w, r, "Net scheduled.", "/nets")
}

func (a *App) handleNetSave(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	me := a.current(r)
	existing, err := a.store.NetByID(id)
	if err != nil {
		a.notFound(w, r)
		return
	}
	n, err := netFromForm(r)
	if err != nil {
		a.fail(w, r, err.Error(), "/nets/"+id+"/edit")
		return
	}
	n.ID = id
	n.CreatedBy = existing.CreatedBy
	n.CreatedAt = existing.CreatedAt
	saved, err := a.store.SaveNet(n)
	if err != nil {
		a.fail(w, r, err.Error(), "/nets/"+id+"/edit")
		return
	}
	a.audit.Write(me.Email, "net.edit", saved.ID, saved.Name, a.clientIP(r))
	a.ok(w, r, "Net updated.", "/nets")
}

func (a *App) handleNetDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	me := a.current(r)
	n, err := a.store.NetByID(id)
	if err != nil {
		a.notFound(w, r)
		return
	}
	if err := a.store.DeleteNet(id); err != nil {
		a.fail(w, r, "Could not delete that net.", "/nets")
		return
	}
	a.audit.Write(me.Email, "net.delete", id, n.Name, a.clientIP(r))
	a.ok(w, r, "Net deleted.", "/nets")
}

var allWeekdays = []time.Weekday{time.Sunday, time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday}

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
