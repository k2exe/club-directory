package main

import (
	"strings"
	"testing"
)

func TestNormalizeColor(t *testing.T) {
	if got := normalizeColor("#2f6690", ""); got != "#2f6690" {
		t.Errorf("preset pick: got %q", got)
	}
	if got := normalizeColor("", "#123abc"); got != "#123abc" {
		t.Errorf("custom hex: got %q", got)
	}
	if got := normalizeColor("", "not-a-color"); got != defaultRoleColor {
		t.Errorf("invalid custom hex should fall back to default, got %q", got)
	}
	if got := normalizeColor("", ""); got != defaultRoleColor {
		t.Errorf("nothing supplied should fall back to default, got %q", got)
	}
}

func TestCustomRoleCRUD(t *testing.T) {
	a := testApp(t)
	role, err := a.store.SaveCustomRole(CustomRole{Name: "Web Admin", Color: "#2f8f8f"})
	if err != nil {
		t.Fatal(err)
	}
	if role.ID == "" {
		t.Fatal("expected an ID to be assigned on create")
	}
	role.Name = "Website Admin"
	if _, err := a.store.SaveCustomRole(role); err != nil {
		t.Fatal(err)
	}
	got, err := a.store.CustomRoleByID(role.ID)
	if err != nil || got.Name != "Website Admin" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if err := a.store.DeleteCustomRole(role.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.CustomRoleByID(role.ID); err != ErrNotFound {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestAssignAndUnassignRoles(t *testing.T) {
	a := testApp(t)
	m := mustCreate(t, a, &Member{Email: "m@example.com", Name: "M", CallSign: "W1M", Role: RoleMember, Status: StatusActive})
	r1, _ := a.store.SaveCustomRole(CustomRole{Name: "Role One", Color: "#2f6690"})
	r2, _ := a.store.SaveCustomRole(CustomRole{Name: "Role Two", Color: "#c8752b"})

	if err := a.store.SetMemberRoles(m.ID, []string{r1.ID, r2.ID}); err != nil {
		t.Fatal(err)
	}
	if got := a.store.RolesFor(m.ID); len(got) != 2 {
		t.Fatalf("expected 2 roles, got %d", len(got))
	}
	// Reassigning replaces the set rather than appending to it.
	if err := a.store.SetMemberRoles(m.ID, []string{r1.ID}); err != nil {
		t.Fatal(err)
	}
	got := a.store.RolesFor(m.ID)
	if len(got) != 1 || got[0].ID != r1.ID {
		t.Fatalf("expected exactly role one after reassignment, got %+v", got)
	}
}

func TestGrantsNetAdmin(t *testing.T) {
	a := testApp(t)
	m := mustCreate(t, a, &Member{Email: "m@example.com", Name: "M", CallSign: "W1M", Role: RoleMember, Status: StatusActive})
	if a.store.GrantsNetAdmin(m.ID) {
		t.Error("a member with no roles should not grant net admin")
	}
	role, _ := a.store.SaveCustomRole(CustomRole{Name: "Net Admin", Color: "#7d5ba6", GrantsNetAdmin: true})
	a.store.SetMemberRoles(m.ID, []string{role.ID})
	if !a.store.GrantsNetAdmin(m.ID) {
		t.Error("holding a grants_net_admin role should grant net admin")
	}
}

// A lifecycle-flagged role's holders are emailed on roster events; a
// non-lifecycle custom role's holders (support-only, e.g. Web Admin) are not.
func TestLifecycleNotificationTargeting(t *testing.T) {
	a := testApp(t)
	lifecycle, _ := a.store.SaveCustomRole(CustomRole{Name: "Echolink Admin", Color: "#2f6690", IsLifecycle: true})
	supportOnly, _ := a.store.SaveCustomRole(CustomRole{Name: "Web Admin", Color: "#2f8f8f"})

	echolinker := mustCreate(t, a, &Member{Email: "echolink@example.com", Name: "E", CallSign: "W1ECH",
		Role: RoleMember, Status: StatusActive})
	webAdmin := mustCreate(t, a, &Member{Email: "webadmin@example.com", Name: "WA", CallSign: "W1WEB",
		Role: RoleMember, Status: StatusActive})
	a.store.SetMemberRoles(echolinker.ID, []string{lifecycle.ID})
	a.store.SetMemberRoles(webAdmin.ID, []string{supportOnly.ID})

	subject := mustCreate(t, a, &Member{Email: "newbie@example.com", Name: "New", CallSign: "W1NEW",
		Role: RoleMember, Status: StatusActive})

	a.notifyLifecycle("was added to the roster", subject, "")

	echolinkMail := readOutboxFor(t, a, "echolink_example.com")
	webAdminMail := readOutboxFor(t, a, "webadmin_example.com")
	if len(echolinkMail) == 0 {
		t.Error("the lifecycle role holder should have been emailed")
	}
	if len(webAdminMail) != 0 {
		t.Error("the ticket-only role holder should NOT have been emailed a lifecycle alert")
	}
}

func readOutboxFor(t *testing.T, a *App, needle string) []string {
	t.Helper()
	entries := outboxFiles(t, a)
	var matches []string
	for _, e := range entries {
		if strings.Contains(e.Name(), needle) {
			matches = append(matches, e.Name())
		}
	}
	return matches
}
