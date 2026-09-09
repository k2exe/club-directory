package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	a := &App{
		cfg:      Config{DataDir: dir, BaseURL: "http://test.local"},
		store:    st,
		mailer:   &Mailer{Outbox: st.OutboxDir()},
		links:    newLinkStore(),
		loginRL:  newLimiter(1000, time.Minute),
		ticketRL: newLimiter(1000, time.Minute),
		audit:    newAuditLog(dir),
	}
	if err := a.loadTemplates(); err != nil {
		t.Fatalf("templates: %v", err)
	}
	return a
}

func mustCreate(t *testing.T, a *App, m *Member) *Member {
	t.Helper()
	out, err := a.store.Create(m)
	if err != nil {
		t.Fatalf("create %s: %v", m.Email, err)
	}
	return out
}

func enrolled() *time.Time { now := time.Now(); return &now }

// get performs a request as the given member (nil for anonymous).
func (a *App) get(t *testing.T, path string, as *Member) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if as != nil {
		req.AddCookie(&http.Cookie{Name: cookieSession, Value: a.issueToken("s", as.ID, as.SessionEpoch, sessionTTL)})
	}
	rec := httptest.NewRecorder()
	a.routes().ServeHTTP(rec, req)
	return rec
}

func (a *App) post(t *testing.T, path string, form url.Values, as *Member) *httptest.ResponseRecorder {
	t.Helper()
	const csrf = "test-csrf-token-long-enough"
	form.Set("csrf", csrf)
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: cookieCSRF, Value: csrf})
	if as != nil {
		req.AddCookie(&http.Cookie{Name: cookieSession, Value: a.issueToken("s", as.ID, as.SessionEpoch, sessionTTL)})
	}
	rec := httptest.NewRecorder()
	a.routes().ServeHTTP(rec, req)
	return rec
}

// ---------- access policy ----------

func TestAccessMatrix(t *testing.T) {
	cases := []struct {
		name string
		m    *Member
		want Access
	}{
		{"anonymous", nil, AccessNone},
		{"banned", &Member{Status: StatusBanned, Role: RoleMember}, AccessNone},
		{"banned admin", &Member{Status: StatusBanned, Role: RoleAdmin, TOTPEnabled: enrolled()}, AccessNone},
		{"silent key", &Member{Status: StatusSK, Role: RoleMember}, AccessNone},
		{"silent key admin", &Member{Status: StatusSK, Role: RoleAdmin, TOTPEnabled: enrolled()}, AccessNone},
		{"pending", &Member{Status: StatusPending, Role: RoleMember}, AccessSelf},
		{"former", &Member{Status: StatusFormer, Role: RoleMember}, AccessSelf},
		{"former admin", &Member{Status: StatusFormer, Role: RoleAdmin, TOTPEnabled: enrolled()}, AccessSelf},
		{"active member", &Member{Status: StatusActive, Role: RoleMember}, AccessMember},
		{"admin without MFA", &Member{Status: StatusActive, Role: RoleAdmin}, AccessSelf},
		{"admin with MFA", &Member{Status: StatusActive, Role: RoleAdmin, TOTPEnabled: enrolled()}, AccessAdmin},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := accessOf(c.m); got != c.want {
				t.Errorf("accessOf = %v, want %v", got, c.want)
			}
		})
	}
}

// ---------- projection ----------

func sharedTarget() *Member {
	return &Member{
		Email: "target@example.com", Name: "Target Person", CallSign: "W1TGT",
		Status: StatusActive, Role: RoleMember,
		ContactEmail: "shared@example.com", Phone: "555-0101",
		Address:    Address{Line1: "1 Elm St", City: "Newington", State: "CT"},
		PhotoFile:  "target.jpg",
		Share:      Share{Email: true, Phone: true, Address: true, Photo: true},
		AdminNotes: "PRIVATE-ADMIN-NOTE",
	}
}

func TestProjectionByViewer(t *testing.T) {
	target := sharedTarget()
	cases := []struct {
		name       string
		viewer     *Member
		wantShared bool // consented fields visible
		wantAdmin  bool // admin-only fields visible
	}{
		{"anonymous", nil, false, false},
		{"pending viewer", &Member{ID: "p", Status: StatusPending, Role: RoleMember}, false, false},
		{"former viewer", &Member{ID: "f", Status: StatusFormer, Role: RoleMember}, false, false},
		{"active member", &Member{ID: "m", Status: StatusActive, Role: RoleMember}, true, false},
		{"admin without MFA", &Member{ID: "a1", Status: StatusActive, Role: RoleAdmin}, false, false},
		{"admin with MFA", &Member{ID: "a2", Status: StatusActive, Role: RoleAdmin, TOTPEnabled: enrolled()}, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := project(target, c.viewer)
			if (got.Phone != "") != c.wantShared {
				t.Errorf("phone visible = %v, want %v", got.Phone != "", c.wantShared)
			}
			if (got.Address.City != "") != c.wantShared {
				t.Errorf("address visible = %v, want %v", got.Address.City != "", c.wantShared)
			}
			if (got.AdminNotes != "") != c.wantAdmin {
				t.Errorf("admin notes visible = %v, want %v", got.AdminNotes != "", c.wantAdmin)
			}
			if (got.LoginEmail != "") != c.wantAdmin {
				t.Errorf("login email visible = %v, want %v", got.LoginEmail != "", c.wantAdmin)
			}
			// Name and call sign are the default share for everyone.
			if got.CallSign != "W1TGT" || got.Name != "Target Person" {
				t.Error("name and call sign should always be projected")
			}
		})
	}
}

func TestUnsharedFieldsNeverProjected(t *testing.T) {
	target := sharedTarget()
	target.Share = Share{} // consent withdrawn across the board
	member := &Member{ID: "m", Status: StatusActive, Role: RoleMember}
	got := project(target, member)
	if got.Phone != "" || got.Email != "" || got.Address.City != "" || got.PhotoFile != "" {
		t.Errorf("unshared fields leaked to a member: %+v", got)
	}
}

func TestSelfAlwaysSeesOwnRecord(t *testing.T) {
	target := sharedTarget()
	target.ID = "self"
	target.Share = Share{}
	got := project(target, target)
	if got.Phone == "" || got.Address.City == "" {
		t.Error("a member should always see their own details")
	}
}

func TestVisibilityByStatus(t *testing.T) {
	set := Settings{PublicDirectory: true, ShowSKPublicly: true, ShowFormerPub: false}
	member := &Member{ID: "m", Status: StatusActive, Role: RoleMember}
	admin := &Member{ID: "a", Status: StatusActive, Role: RoleAdmin, TOTPEnabled: enrolled()}
	for _, c := range []struct {
		status        Status
		pub, mem, adm bool
	}{
		{StatusActive, true, true, true},
		{StatusSK, true, true, true},
		{StatusFormer, false, true, true},
		{StatusPending, false, false, true},
		{StatusBanned, false, false, true},
	} {
		target := &Member{ID: "t", Name: "N", CallSign: "W1X", Status: c.status}
		if got := visible(target, nil, set); got != c.pub {
			t.Errorf("%s public visible = %v, want %v", c.status, got, c.pub)
		}
		if got := visible(target, member, set); got != c.mem {
			t.Errorf("%s member visible = %v, want %v", c.status, got, c.mem)
		}
		if got := visible(target, admin, set); got != c.adm {
			t.Errorf("%s admin visible = %v, want %v", c.status, got, c.adm)
		}
	}
}

// ---------- regressions ----------

// An admin who has not enrolled in two-step verification must not gain admin
// sight through routes that sit outside the /admin middleware.
func TestRegressionAdminMFABypassOnPublicRoutes(t *testing.T) {
	a := testApp(t)
	target := mustCreate(t, a, sharedTarget())
	unenrolled := mustCreate(t, a, &Member{
		Email: "new-admin@example.com", Name: "New Admin", CallSign: "W1ADM",
		Role: RoleAdmin, Status: StatusActive,
	})
	for _, path := range []string{"/", "/m/" + target.ID} {
		body := a.get(t, path, unenrolled).Body.String()
		if strings.Contains(body, "PRIVATE-ADMIN-NOTE") {
			t.Errorf("%s leaked admin notes to an unenrolled admin", path)
		}
		if strings.Contains(body, "target@example.com") {
			t.Errorf("%s leaked the sign-in email to an unenrolled admin", path)
		}
		if strings.Contains(body, "555-0101") {
			t.Errorf("%s leaked a member's phone to an unenrolled admin", path)
		}
	}
	if code := a.get(t, "/admin", unenrolled).Code; code != http.StatusSeeOther {
		t.Errorf("/admin returned %d, want a redirect to enrolment", code)
	}
}

// A self-registered account awaiting approval must not read the roster.
func TestRegressionPendingSignupCannotReadSharedDetails(t *testing.T) {
	a := testApp(t)
	target := mustCreate(t, a, sharedTarget())
	pending := mustCreate(t, a, &Member{
		Email: "stranger@example.com", Name: "Stranger", CallSign: "W9STR",
		Role: RoleMember, Status: StatusPending,
	})
	body := a.get(t, "/m/"+target.ID, pending).Body.String()
	for _, secret := range []string{"555-0101", "Newington", "shared@example.com"} {
		if strings.Contains(body, secret) {
			t.Errorf("pending signup could read %q", secret)
		}
	}
	if body := a.get(t, "/", pending).Body.String(); strings.Contains(body, "555-0101") {
		t.Error("pending signup could read shared details from the listing")
	}
	// They must still be able to complete their own record.
	if code := a.get(t, "/account", pending).Code; code != http.StatusOK {
		t.Errorf("pending signup got %d on /account, want 200", code)
	}
}

// Changing a contact detail as an admin withdraws the consent that covered
// the old value, so the new one is not published on the member's behalf.
func TestRegressionAdminEditWithdrawsConsent(t *testing.T) {
	a := testApp(t)
	target := mustCreate(t, a, sharedTarget())
	admin := mustCreate(t, a, &Member{
		Email: "boss@example.com", Name: "Boss", CallSign: "W1BSS",
		Role: RoleAdmin, Status: StatusActive, TOTPEnabled: enrolled(), TOTPSecret: newTOTPSecret(),
	})
	form := url.Values{
		"name": {"Target Person"}, "call_sign": {"W1TGT"}, "email": {"target@example.com"},
		"role": {"member"}, "contact_email": {"shared@example.com"},
		"phone": {"555-9999"}, // changed
		"line1": {"1 Elm St"}, "city": {"Newington"}, "state": {"CT"},
	}
	if code := a.post(t, "/admin/member/"+target.ID, form, admin).Code; code != http.StatusSeeOther {
		t.Fatalf("admin save returned %d", code)
	}
	after, _ := a.store.ByID(target.ID)
	if after.Phone != "555-9999" {
		t.Fatalf("admin edit did not store the new phone: %q", after.Phone)
	}
	if after.Share.Phone {
		t.Error("consent for the phone survived an admin changing it")
	}
	if !after.Share.Address || !after.Share.Email {
		t.Error("consent was withdrawn for fields the admin did not change")
	}
	if !after.NeedsReview {
		t.Error("record should be flagged for member review")
	}
	// The new number must be invisible to other members until approved.
	other := mustCreate(t, a, &Member{Email: "other@example.com", Name: "Other", CallSign: "W2OTH",
		Role: RoleMember, Status: StatusActive})
	if body := a.get(t, "/m/"+target.ID, other).Body.String(); strings.Contains(body, "555-9999") {
		t.Error("the replacement phone number was published without consent")
	}
}

func TestBannedAndSKSessionsAreDead(t *testing.T) {
	a := testApp(t)
	for _, st := range []Status{StatusBanned, StatusSK} {
		m := mustCreate(t, a, &Member{Email: string(st) + "@example.com", Name: "X", CallSign: "W1" + strings.ToUpper(string(st)),
			Role: RoleMember, Status: st})
		if code := a.get(t, "/account", m).Code; code != http.StatusSeeOther {
			t.Errorf("%s account access returned %d, want redirect", st, code)
		}
	}
}

// ---------- units ----------

func TestCSVSafe(t *testing.T) {
	for in, want := range map[string]string{
		"":                "",
		"Hiram Maxim":     "Hiram Maxim",
		"=1+1":            "'=1+1",
		"+44 7700 900000": "'+44 7700 900000",
		"-lookup":         "'-lookup",
		"@SUM(A1)":        "'@SUM(A1)",
		"W1AW":            "W1AW",
	} {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTOTPRoundTripAndReplay(t *testing.T) {
	secret := newTOTPSecret()
	now := time.Now().Unix()
	code, err := totpAt(secret, now/totpPeriod)
	if err != nil {
		t.Fatal(err)
	}
	step, ok := verifyTOTP(secret, code, now, 0)
	if !ok {
		t.Fatal("a freshly generated code was rejected")
	}
	if _, ok := verifyTOTP(secret, code, now, step); ok {
		t.Error("a code was accepted twice")
	}
	if _, ok := verifyTOTP(secret, "000000", now, 0); ok {
		t.Error("an arbitrary code was accepted")
	}
}

func TestBackupCodesAreSingleUse(t *testing.T) {
	plain, hashed := newBackupCodes()
	if len(plain) != backupCodeCount || len(hashed) != backupCodeCount {
		t.Fatalf("got %d codes", len(plain))
	}
	for _, h := range hashed {
		if strings.Contains(strings.Join(plain, " "), h) {
			t.Fatal("a backup code was stored in the clear")
		}
	}
	rest, ok := useBackupCode(hashed, plain[3])
	if !ok || len(rest) != backupCodeCount-1 {
		t.Fatalf("code not consumed: ok=%v left=%d", ok, len(rest))
	}
	if _, ok := useBackupCode(rest, plain[3]); ok {
		t.Error("a backup code worked twice")
	}
	if _, ok := useBackupCode(rest, "AAAAA-BBBBB"); ok {
		t.Error("an invented backup code was accepted")
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/admin":              "/admin",
		"/m/abc":              "/m/abc",
		"//evil.example.com":  "",
		"https://evil.test":   "",
		"javascript:alert(1)": "",
		"":                    "",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTrustedProxyIP(t *testing.T) {
	nets, err := parseProxies("10.0.0.1, 192.168.1.0/24")
	if err != nil {
		t.Fatal(err)
	}
	a := &App{trustedProxies: nets}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 192.168.1.5")

	req.RemoteAddr = "10.0.0.1:1234" // a trusted proxy: believe the header
	if got := a.clientIP(req); got != "203.0.113.9" {
		t.Errorf("behind trusted proxy got %q, want 203.0.113.9", got)
	}
	req.RemoteAddr = "198.51.100.7:1234" // untrusted peer: ignore the header
	if got := a.clientIP(req); got != "198.51.100.7" {
		t.Errorf("untrusted peer got %q, want its own address", got)
	}
}

func TestPhotoRejectsNonImageAndBombs(t *testing.T) {
	dir := t.TempDir()
	if _, err := savePhoto(strings.NewReader("#!/bin/sh\necho not an image"), dir, "m1"); err == nil {
		t.Error("a text file was accepted as a photo")
	}
	// A PNG header declaring 60000x60000 pixels: small file, huge decode.
	bomb := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 13, 'I', 'H', 'D', 'R',
		0, 0, 0xEA, 0x60, 0, 0, 0xEA, 0x60, 8, 2, 0, 0, 0}
	if _, err := savePhoto(strings.NewReader(string(bomb)), dir, "m2"); err == nil {
		t.Error("an oversized image was accepted")
	}
}

func TestQREncodesStably(t *testing.T) {
	m, err := QREncode("otpauth://totp/Club:a@b.c?issuer=Club&secret=JBSWY3DPEHPK3PXP")
	if err != nil {
		t.Fatal(err)
	}
	if m.size != 33 { // version 4
		t.Errorf("size = %d, want 33", m.size)
	}
	dark := 0
	for _, v := range m.mod {
		if v {
			dark++
		}
	}
	// Locked to the output verified against an independent encoder and decoder.
	if dark != 567 {
		t.Errorf("dark modules = %d, want 567 (encoder output changed)", dark)
	}
	if _, err := QREncode(strings.Repeat("x", 300)); err == nil {
		t.Error("an over-long payload should be refused, not silently truncated")
	}
}
