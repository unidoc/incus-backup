package auth

import (
	"reflect"
	"testing"
)

// Pure parts of the single sign-on and passkey code (the flows themselves are tested end to end
// through HTTP in internal/server/mgmt: authn_webauthn_test.go, authn_oidc_test.go).

func TestSafeNextKeepsTheBrowserOnThisSite(t *testing.T) {
	for in, want := range map[string]string{
		"/repos?x=1": "/repos?x=1", "": "/", "https://evil.example": "/", "//evil.example/x": "/", "/\\evil.example": "/",
		"/api/v1/session": "/", "/ok\r\nSet-Cookie: x": "/", "repos": "/",
		"/\t/evil.example/": "/", "/\n/evil": "/", "/a\x00b": "/", "/a\x7fb": "/", "/\x1f/evil": "/", "/jobs?x=1": "/jobs?x=1",
	} {
		if got := SafeNext(in); got != want {
			t.Errorf("SafeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEmailVerifiedIsFailClosed(t *testing.T) {
	for v, want := range map[any]bool{true: true, "true": true, "TRUE": true, false: false, "false": false, "yes": false, 1.0: false, nil: false} {
		if verifiedEmail(v) != want {
			t.Errorf("email_verified %#v: want %v", v, want)
		}
	}
}

func TestGroupsClaim(t *testing.T) {
	raw := map[string]any{"groups": []any{"a", 7, "b"}, "role": "ops"}
	if g := groupsFrom(raw, "groups"); !reflect.DeepEqual(g, []string{"a", "b"}) {
		t.Fatalf("%v", g)
	}
	if g := groupsFrom(raw, "role"); !reflect.DeepEqual(g, []string{"ops"}) {
		t.Fatalf("%v", g)
	}
	if g := groupsFrom(raw, "missing"); g != nil {
		t.Fatalf("%v", g)
	}
}

func TestProviderAccessMapping(t *testing.T) {
	p := &OIDCProvider{DefaultRole: None,
		RoleMappings:  []RoleMapping{{Group: "ops", Role: Operator}, {Group: "admins", Role: Admin}, {Group: "view", Role: ReadOnly}},
		ScopeMappings: []ScopeMapping{{Group: "team-a", Repository: "r1", Role: Restore}, {Group: "team-b", Repository: "r1", Role: Operator}, {Group: "team-b", Repository: "r2", Role: ReadOnly}}}
	cases := []struct {
		groups []string
		role   Role
		scopes map[string]Role
	}{
		{nil, None, map[string]Role{}},                                                        // nothing: no access at all
		{[]string{"view", "ops"}, Operator, map[string]Role{}},                                // the highest role wins
		{[]string{"team-a", "team-b"}, None, map[string]Role{"r1": Operator, "r2": ReadOnly}}, // the highest per repository
		{[]string{"admins", "team-a"}, Admin, nil},                                            // admins are never restricted
	}
	for _, c := range cases {
		role, scopes := p.access(c.groups)
		if role != c.role || !reflect.DeepEqual(scopes, c.scopes) {
			t.Errorf("%v: %s %v, want %s %v", c.groups, role, scopes, c.role, c.scopes)
		}
	}
	// no repository mappings: not restricted
	p.ScopeMappings = nil
	if _, scopes := p.access([]string{"team-a"}); scopes != nil {
		t.Fatalf("unrestricted provider restricted: %v", scopes)
	}
}

func TestProviderInputValidation(t *testing.T) {
	ok := OIDCProviderInput{Name: "corp", Issuer: "https://id.example.com/realms/x/", ClientID: "c"}
	if err := ok.normalize(); err != nil || ok.Issuer != "https://id.example.com/realms/x/" || !reflect.DeepEqual(ok.Scopes, []string{"openid", "email", "profile"}) || ok.DefaultRole != None {
		t.Fatalf("%v %+v", err, ok)
	}
	for _, bad := range []OIDCProviderInput{
		{Name: "Corp", Issuer: "https://x", ClientID: "c"},
		{Name: "corp", Issuer: "http://id.example.com", ClientID: "c"},
		{Name: "corp", Issuer: "https://x", ClientID: ""},
		{Name: "corp", Issuer: "https://x", ClientID: "c", DefaultRole: Admin},
		{Name: "corp", Issuer: "https://x", ClientID: "c", RoleMappings: []RoleMapping{{Group: "", Role: Admin}}},
		{Name: "corp", Issuer: "https://x", ClientID: "c", ScopeMappings: []ScopeMapping{{Group: "g", Repository: "r", Role: None}}},
	} {
		if err := bad.normalize(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	loop := OIDCProviderInput{Name: "dev", Issuer: "http://127.0.0.1:5556/dex", ClientID: "c"}
	if err := loop.normalize(); err != nil {
		t.Fatalf("loopback http issuer: %v", err)
	}
}

func TestPolicyCovers(t *testing.T) {
	if !policyCovers("all", ReadOnly) || !policyCovers("admin", Admin) || policyCovers("admin", Operator) || policyCovers("none", Admin) || policyCovers("", Admin) {
		t.Fatal("policyCovers")
	}
}

func TestOriginValidation(t *testing.T) {
	for o, ok := range map[string]bool{"https://backup.example.com": true, "https://backup.example.com:8443": true, "http://localhost:5173": true,
		"http://backup.example.com": false, "https://backup.example.com/path": false, "backup.example.com": false} {
		if (validOrigin(o) == nil) != ok {
			t.Errorf("origin %s: want ok=%v", o, ok)
		}
	}
}
