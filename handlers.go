package main

import (
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type page struct {
	Title string
	Me    *Member
	Set   Settings
	CSRF  string
	Flash *flashMsg
	Path  string
	Admin bool // true only for an enrolled, active admin
	Data  any
}

func (a *App) render(w http.ResponseWriter, r *http.Request, name, title string, data any) {
	t, ok := a.tmpl[name]
	if !ok {
		http.Error(w, "template missing: "+name, http.StatusInternalServerError)
		return
	}
	me := a.current(r)
	p := page{
		Title: title,
		Me:    me,
		Admin: accessOf(me) == AccessAdmin,
		Set:   a.store.Settings(),
		CSRF:  a.csrfToken(w, r),
		Flash: a.takeFlash(w, r),
		Path:  r.URL.Path,
		Data:  data,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(w, p); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func (a *App) fail(w http.ResponseWriter, r *http.Request, msg, redirect string) {
	a.flash(w, "error", msg)
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

func (a *App) ok(w http.ResponseWriter, r *http.Request, msg, redirect string) {
	a.flash(w, "ok", msg)
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

// ---------- middleware ----------

// mfaExempt lists the paths an admin may reach before enrolling in MFA.
var mfaExempt = map[string]bool{
	"/account/security":        true,
	"/account/security/start":  true,
	"/account/security/enable": true,
	"/account/security/codes":  true,
}

func (a *App) requireMember(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := a.current(r)
		if accessOf(m) == AccessNone {
			http.Redirect(w, r, "/login?next="+template.URLQueryEscaper(r.URL.Path), http.StatusSeeOther)
			return
		}
		// Two-step verification is mandatory for admins, optional for members.
		if m.IsAdmin() && !m.MFAOn() && !mfaExempt[r.URL.Path] {
			a.flash(w, "error", "Admin accounts need two-step verification. Set it up to continue.")
			http.Redirect(w, r, "/account/security", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost && !a.checkCSRF(r) {
			http.Error(w, "This form expired. Go back, reload the page and try again.", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

func (a *App) requireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return a.requireMember(func(w http.ResponseWriter, r *http.Request) {
		if accessOf(a.current(r)) != AccessAdmin {
			http.Error(w, "Admins only.", http.StatusForbidden)
			return
		}
		h(w, r)
	})
}

// ---------- public directory ----------

type dirData struct {
	Cards      []Card
	Query      Query
	Total      int
	Groups     []statusGroup
	CanSeeMore bool
}

type statusGroup struct {
	Status Status
	Cards  []Card
}

func (a *App) handleDirectory(w http.ResponseWriter, r *http.Request) {
	me := a.current(r)
	set := a.store.Settings()
	if me == nil && !set.PublicDirectory {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	q := Query{
		Text:   r.URL.Query().Get("q"),
		Status: r.URL.Query().Get("status"),
	}
	cards := a.store.Search(me, q)

	// Active members are the directory proper; former members and silent keys
	// are listed separately because they mean different things to a club.
	groups := []statusGroup{}
	for _, s := range []Status{StatusActive, StatusFormer, StatusSK, StatusPending, StatusBanned} {
		g := statusGroup{Status: s}
		for _, c := range cards {
			if c.Status == s {
				g.Cards = append(g.Cards, c)
			}
		}
		if len(g.Cards) > 0 {
			groups = append(groups, g)
		}
	}
	a.render(w, r, "directory", set.ClubName, dirData{
		Cards: cards, Query: q, Total: len(cards), Groups: groups,
		CanSeeMore: me == nil,
	})
}

func (a *App) handleMember(w http.ResponseWriter, r *http.Request) {
	me := a.current(r)
	set := a.store.Settings()
	if me == nil && !set.PublicDirectory {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	m, err := a.store.ByID(r.PathValue("id"))
	if err != nil || !visible(m, me, set) {
		a.notFound(w, r)
		return
	}
	c := project(m, me)
	a.render(w, r, "member", c.Name+" "+c.CallSign, c)
}

func (a *App) notFound(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	a.render(w, r, "notfound", "Not found", nil)
}

func (a *App) handlePhoto(w http.ResponseWriter, r *http.Request) {
	file := filepath.Base(r.PathValue("file"))
	me := a.current(r)
	set := a.store.Settings()
	var owner *Member
	for _, m := range a.store.All() {
		if m.PhotoFile == file {
			owner = m
			break
		}
	}
	if owner == nil || !visible(owner, me, set) {
		http.NotFound(w, r)
		return
	}
	if project(owner, me).PhotoFile == "" {
		http.NotFound(w, r) // stored, but not shared with this viewer
		return
	}
	f, err := os.Open(filepath.Join(a.store.PhotoDir(), file))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=300")
	http.ServeContent(w, r, file, st.ModTime(), f)
}

// ---------- authentication ----------

func (a *App) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if a.current(r) != nil {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	a.render(w, r, "login", "Sign in", r.URL.Query().Get("next"))
}

func (a *App) handleLoginRequest(w http.ResponseWriter, r *http.Request) {
	if !a.checkCSRF(r) {
		a.fail(w, r, "That form expired. Try again.", "/login")
		return
	}
	next := safeNext(r.FormValue("next"))
	email := normalizeEmail(r.FormValue("email"))
	if email == "" || !strings.Contains(email, "@") {
		a.fail(w, r, "Enter a valid email address.", "/login")
		return
	}
	if !a.loginRL.allow("ip:"+a.clientIP(r)) || !a.loginRL.allow("em:"+email) {
		a.fail(w, r, "Too many sign-in requests. Wait a few minutes and try again.", "/login")
		return
	}

	set := a.store.Settings()
	m, err := a.store.ByEmail(email)
	switch {
	case errors.Is(err, ErrNotFound) && set.SignupsOpen:
		m, err = a.store.Create(&Member{Email: email, Role: RoleMember, Status: StatusPending})
		if err != nil {
			a.fail(w, r, "Could not start your sign-up. Ask an admin for help.", "/login")
			return
		}
		a.audit.Write(email, "member.signup", m.ID, "self sign-up", a.clientIP(r))
		a.notifyStaffNewMember(m)
		a.sendLink(m, "signup", next)
	case err == nil && accessOf(m) != AccessNone:
		a.sendLink(m, "login", next)
	default:
		// Unknown address with sign-ups closed, or a banned account. Say
		// nothing either way — the response must not reveal who is on the
		// roster to someone who is not signed in.
		a.audit.Write(email, "login.rejected", "", string(statusOf(m)), a.clientIP(r))
	}
	http.Redirect(w, r, "/login/sent", http.StatusSeeOther)
}

func statusOf(m *Member) Status {
	if m == nil {
		return "unknown-address"
	}
	return m.Status
}

// safeNext keeps the post-sign-in redirect on this site.
func safeNext(p string) string {
	if strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && !strings.Contains(p, "\\") {
		return p
	}
	return ""
}

func (a *App) sendLink(m *Member, purpose, next string) {
	tok := a.links.issue(m.ID, purpose, next, linkTTL)
	url := fmt.Sprintf("%s/auth/%s", a.cfg.BaseURL, tok)
	set := a.store.Settings()
	subject := "Your sign-in link for " + set.ClubName
	body := fmt.Sprintf(`Hello,

Use this link to sign in to %s:

%s

The link works once and expires in %d minutes. If you did not ask to sign in,
you can ignore this message.

%s
`, set.ClubName, url, int(linkTTL.Minutes()), set.ClubName)
	if purpose == "invite" {
		subject = "You have been added to " + set.ClubName
		body = fmt.Sprintf(`Hello,

An admin added you to %s. Use this link to sign in and choose
what you would like to share:

%s

The link works once and expires in %d minutes.

%s
`, set.ClubName, url, int(linkTTL.Minutes()), set.ClubName)
	}
	if err := a.mailer.Send(m.Email, subject, body); err != nil {
		log.Printf("sending link to %s: %v", m.Email, err)
	}
}

func (a *App) handleLinkSent(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "linksent", "Check your email", a.mailer.Configured())
}

func (a *App) handleMagicLink(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.links.consume(r.PathValue("token"))
	if !ok {
		a.fail(w, r, "That link has expired or was already used. Request a new one.", "/login")
		return
	}
	m, err := a.store.ByID(rec.MemberID)
	if err != nil || accessOf(m) == AccessNone {
		a.fail(w, r, "That link is no longer valid.", "/login")
		return
	}
	if m.MFAOn() {
		a.setCookie(w, cookiePending, a.issueToken("p", m.ID, m.SessionEpoch, pendingTTL), pendingTTL)
		a.setCookie(w, cookieNext, rec.Next, pendingTTL)
		http.Redirect(w, r, "/login/code", http.StatusSeeOther)
		return
	}
	a.login(w, m)
	a.audit.Write(m.Email, "login.success", m.ID, "magic link", a.clientIP(r))
	if m.Name == "" || m.CallSign == "" {
		a.ok(w, r, "Welcome. Add your name and call sign to finish setting up.", "/account")
		return
	}
	http.Redirect(w, r, landing(rec.Next), http.StatusSeeOther)
}

func (a *App) handleMFAForm(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(cookiePending)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if _, ok := a.readToken("p", c.Value); !ok {
		a.fail(w, r, "That sign-in attempt timed out. Request a new link.", "/login")
		return
	}
	a.render(w, r, "mfa", "Enter your code", nil)
}

func (a *App) handleMFASubmit(w http.ResponseWriter, r *http.Request) {
	if !a.checkCSRF(r) {
		a.fail(w, r, "That form expired. Try again.", "/login")
		return
	}
	c, err := r.Cookie(cookiePending)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	m, ok := a.readToken("p", c.Value)
	if !ok {
		a.fail(w, r, "That sign-in attempt timed out. Request a new link.", "/login")
		return
	}
	if !a.loginRL.allow("mfa:" + m.ID) {
		a.fail(w, r, "Too many attempts. Wait a few minutes and start again.", "/login")
		return
	}
	code := strings.TrimSpace(r.FormValue("code"))

	if step, valid := verifyTOTP(m.TOTPSecret, code, time.Now().Unix(), m.TOTPLastStep); valid {
		a.store.Update(m.ID, func(x *Member) error { x.TOTPLastStep = step; return nil })
		a.login(w, m)
		a.audit.Write(m.Email, "login.success", m.ID, "magic link + authenticator", a.clientIP(r))
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	if rest, used := useBackupCode(m.BackupCodes, code); used {
		a.store.Update(m.ID, func(x *Member) error { x.BackupCodes = rest; return nil })
		a.login(w, m)
		a.audit.Write(m.Email, "login.success", m.ID,
			fmt.Sprintf("magic link + backup code (%d left)", len(rest)), a.clientIP(r))
		a.ok(w, r, fmt.Sprintf("Signed in with a backup code. %d codes left.", len(rest)), "/account/security")
		return
	}
	a.audit.Write(m.Email, "login.badcode", m.ID, "", a.clientIP(r))
	a.fail(w, r, "That code was not right. Check your authenticator and try again.", "/login/code")
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !a.checkCSRF(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.clearCookie(w, cookieSession)
	a.clearCookie(w, cookiePending)
	a.ok(w, r, "Signed out.", "/")
}

// ---------- member self-service ----------

type accountData struct {
	Me    *Member
	Photo string
}

func (a *App) handleAccount(w http.ResponseWriter, r *http.Request) {
	m := a.current(r)
	a.render(w, r, "account", "Your details", accountData{Me: m, Photo: m.PhotoFile})
}

func (a *App) handleAccountSave(w http.ResponseWriter, r *http.Request) {
	m := a.current(r)
	name := strings.TrimSpace(r.FormValue("name"))
	call := strings.ToUpper(strings.TrimSpace(r.FormValue("call_sign")))
	if name == "" {
		a.fail(w, r, "Your name cannot be blank.", "/account")
		return
	}
	_, err := a.store.Update(m.ID, func(x *Member) error {
		x.Name = name
		x.CallSign = call
		x.ContactEmail = normalizeEmail(r.FormValue("contact_email"))
		x.Phone = strings.TrimSpace(r.FormValue("phone"))
		x.Address = Address{
			Line1:  strings.TrimSpace(r.FormValue("line1")),
			Line2:  strings.TrimSpace(r.FormValue("line2")),
			City:   strings.TrimSpace(r.FormValue("city")),
			State:  strings.TrimSpace(r.FormValue("state")),
			Postal: strings.TrimSpace(r.FormValue("postal")),
		}
		x.NeedsReview = false
		return nil
	})
	if err != nil {
		a.fail(w, r, "Could not save your details.", "/account")
		return
	}
	a.audit.Write(m.Email, "member.self_edit", m.ID, "", a.clientIP(r))
	a.ok(w, r, "Saved.", "/account")
}

// handleSharingSave is the consent step. Only the member themselves can reach
// it, so information an admin typed in stays private until its owner says yes.
func (a *App) handleSharingSave(w http.ResponseWriter, r *http.Request) {
	m := a.current(r)
	on := func(k string) bool { return r.FormValue(k) != "" }
	_, err := a.store.Update(m.ID, func(x *Member) error {
		x.Share = Share{
			Email:   on("share_email"),
			Phone:   on("share_phone"),
			Address: on("share_address"),
			Photo:   on("share_photo"),
		}
		x.NeedsReview = false
		return nil
	})
	if err != nil {
		a.fail(w, r, "Could not save your sharing choices.", "/account")
		return
	}
	a.audit.Write(m.Email, "member.sharing", m.ID, "consent updated", a.clientIP(r))
	a.ok(w, r, "Sharing preferences saved.", "/account")
}

func (a *App) handlePhotoUpload(w http.ResponseWriter, r *http.Request) {
	m := a.current(r)
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBody)
	if err := r.ParseMultipartForm(2 << 20); err != nil {
		a.fail(w, r, "That image is too large. The limit is 6 MB.", "/account")
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			r.MultipartForm.RemoveAll() // drop anything spilled to temp files
		}
	}()
	f, _, err := r.FormFile("photo")
	if err != nil {
		a.fail(w, r, "Choose an image file first.", "/account")
		return
	}
	defer f.Close()
	name, err := savePhoto(f, a.store.PhotoDir(), m.ID)
	if err != nil {
		a.fail(w, r, err.Error(), "/account")
		return
	}
	old := m.PhotoFile
	a.store.Update(m.ID, func(x *Member) error { x.PhotoFile = name; return nil })
	if old != "" {
		os.Remove(filepath.Join(a.store.PhotoDir(), old))
	}
	a.audit.Write(m.Email, "member.photo", m.ID, "uploaded", a.clientIP(r))
	a.ok(w, r, "Photo uploaded. Tick “Photo” below to show it in the directory.", "/account")
}

func (a *App) handlePhotoRemove(w http.ResponseWriter, r *http.Request) {
	m := a.current(r)
	if m.PhotoFile != "" {
		os.Remove(filepath.Join(a.store.PhotoDir(), m.PhotoFile))
	}
	a.store.Update(m.ID, func(x *Member) error {
		x.PhotoFile = ""
		x.Share.Photo = false
		return nil
	})
	a.ok(w, r, "Photo removed.", "/account")
}

// bumpAndReissue invalidates every existing session for a member, then hands
// the browser making the change a fresh cookie so it stays signed in.
func (a *App) bumpAndReissue(w http.ResponseWriter, id string) {
	updated, err := a.store.Update(id, func(x *Member) error { x.SessionEpoch++; return nil })
	if err == nil {
		a.setCookie(w, cookieSession, a.issueToken("s", updated.ID, updated.SessionEpoch, sessionTTL), sessionTTL)
	}
}

// ---------- two-step verification ----------

type securityData struct {
	Me          *Member
	Secret      string
	Grouped     string
	QR          template.HTML
	Setup       bool
	BackupCodes []string
	Required    bool
}

func (a *App) handleSecurity(w http.ResponseWriter, r *http.Request) {
	m := a.current(r)
	a.render(w, r, "security", "Sign-in security", securityData{Me: m, Required: m.IsAdmin()})
}

// handleMFAStart shows the enrolment code. The secret is only written to the
// record once the member proves they can generate a valid code from it.
func (a *App) handleMFAStart(w http.ResponseWriter, r *http.Request) {
	m := a.current(r)
	secret := newTOTPSecret()
	set := a.store.Settings()
	issuer := set.ClubName
	if issuer == "" {
		issuer = "Club Directory"
	}
	uri := otpauthURI(issuer, m.Email, secret)
	svg, err := qrSVG(uri)
	if err != nil {
		svg = ""
	}
	a.render(w, r, "security", "Set up two-step verification", securityData{
		Me: m, Secret: secret, Grouped: groupSecret(secret),
		QR: template.HTML(svg), Setup: true, Required: m.IsAdmin(),
	})
}

func (a *App) handleMFAEnable(w http.ResponseWriter, r *http.Request) {
	m := a.current(r)
	secret := strings.TrimSpace(r.FormValue("secret"))
	code := strings.TrimSpace(r.FormValue("code"))
	step, valid := verifyTOTP(secret, code, time.Now().Unix(), 0)
	if !valid {
		a.fail(w, r, "That code did not match. Check the time on your phone and try again.", "/account/security")
		return
	}
	plain, hashed := newBackupCodes()
	now := time.Now()
	if _, err := a.store.Update(m.ID, func(x *Member) error {
		x.TOTPSecret = secret
		x.TOTPEnabled = &now
		x.TOTPLastStep = step
		x.BackupCodes = hashed
		return nil
	}); err != nil {
		a.fail(w, r, "Could not turn on two-step verification.", "/account/security")
		return
	}
	a.bumpAndReissue(w, m.ID) // other browsers lose their session
	a.audit.Write(m.Email, "mfa.enabled", m.ID, "", a.clientIP(r))
	a.render(w, r, "security", "Two-step verification is on", securityData{
		Me: m, BackupCodes: plain, Required: m.IsAdmin(),
	})
}

func (a *App) handleMFADisable(w http.ResponseWriter, r *http.Request) {
	m := a.current(r)
	if m.IsAdmin() {
		a.fail(w, r, "Admin accounts must keep two-step verification on.", "/account/security")
		return
	}
	a.store.Update(m.ID, func(x *Member) error {
		x.TOTPSecret = ""
		x.TOTPEnabled = nil
		x.TOTPLastStep = 0
		x.BackupCodes = nil
		return nil
	})
	a.bumpAndReissue(w, m.ID)
	a.audit.Write(m.Email, "mfa.disabled", m.ID, "", a.clientIP(r))
	a.ok(w, r, "Two-step verification is off. You will sign in with an email link only.", "/account/security")
}

func (a *App) handleNewBackupCodes(w http.ResponseWriter, r *http.Request) {
	m := a.current(r)
	if !m.MFAOn() {
		a.fail(w, r, "Turn on two-step verification first.", "/account/security")
		return
	}
	plain, hashed := newBackupCodes()
	a.store.Update(m.ID, func(x *Member) error { x.BackupCodes = hashed; return nil })
	a.bumpAndReissue(w, m.ID)
	a.audit.Write(m.Email, "mfa.backup_codes", m.ID, "regenerated", a.clientIP(r))
	a.render(w, r, "security", "New backup codes", securityData{
		Me: m, BackupCodes: plain, Required: m.IsAdmin(),
	})
}

func (a *App) handleRevokeSessions(w http.ResponseWriter, r *http.Request) {
	m := a.current(r)
	a.store.Update(m.ID, func(x *Member) error { x.SessionEpoch++; return nil })
	a.clearCookie(w, cookieSession)
	a.audit.Write(m.Email, "session.revoke_all", m.ID, "", a.clientIP(r))
	a.ok(w, r, "Signed out everywhere. Sign in again when you are ready.", "/login")
}
