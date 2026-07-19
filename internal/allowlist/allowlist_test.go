package allowlist

import "testing"

func mustNew(t *testing.T, entries ...string) Allowlist {
	t.Helper()
	a, err := New(entries)
	if err != nil {
		t.Fatalf("New(%v): %v", entries, err)
	}
	return a
}

func TestAllows(t *testing.T) {
	tests := []struct {
		name    string
		entries []string
		url     string
		want    bool
	}{
		{"empty list denies (fail-closed)", nil, "https://gitlab.example.com/acme/app.git", false},
		{"wildcard allows anything", []string{"*"}, "https://anything.example.com/x.git", true},
		{"wildcard among others", []string{"https://only.example.com", "*"}, "https://other.example.com/x", true},

		{"exact match", []string{"https://gl.example.com/acme/app.git"}, "https://gl.example.com/acme/app.git", true},
		{"exact match, entry without .git", []string{"https://gl.example.com/acme/app"}, "https://gl.example.com/acme/app.git", true},
		{"exact match, url without .git", []string{"https://gl.example.com/acme/app.git"}, "https://gl.example.com/acme/app", true},
		{"host prefix", []string{"https://gl.example.com"}, "https://gl.example.com/acme/app.git", true},
		{"group prefix", []string{"https://gl.example.com/acme"}, "https://gl.example.com/acme/app.git", true},

		{"deny sibling group", []string{"https://gl.example.com/acme"}, "https://gl.example.com/acmecorp/app.git", false},
		{"deny look-alike host", []string{"https://gl.example.com"}, "https://gl.example.com.evil.com/x.git", false},
		{"deny userinfo confusion", []string{"https://gl.example.com"}, "https://gl.example.com@evil.com/x.git", false},
		{"deny scheme downgrade", []string{"https://gl.example.com"}, "http://gl.example.com/acme/app.git", false},
		{"deny different host", []string{"https://gl.example.com"}, "https://evil.example.com/x.git", false},

		{"default https port unified", []string{"https://gl.example.com/acme"}, "https://gl.example.com:443/acme/app.git", true},
		{"non-default port must match", []string{"https://gl.example.com"}, "https://gl.example.com:8443/acme/app.git", false},
		{"host case-insensitive", []string{"https://GL.Example.com"}, "https://gl.example.com/acme/app.git", true},
		{"path case-sensitive", []string{"https://gl.example.com/Acme"}, "https://gl.example.com/acme/app.git", false},
		{"trailing slash on entry", []string{"https://gl.example.com/acme/"}, "https://gl.example.com/acme/app.git", true},

		{"scp-style opaque prefix", []string{"git@gl.example.com:acme"}, "git@gl.example.com:acme/app.git", true},
		{"local path prefix", []string{"/srv/git/acme"}, "/srv/git/acme/app.git", true},
		{"local path denies sibling", []string{"/srv/git/acme"}, "/srv/git/acmecorp/app.git", false},

		{"deny URL dot-dot traversal", []string{"https://gl.example.com/acme"}, "https://gl.example.com/acme/../evil/app.git", false},
		{"deny URL single-dot segment", []string{"https://gl.example.com/acme"}, "https://gl.example.com/acme/./app.git", false},
		{"deny percent-encoded traversal", []string{"https://gl.example.com/acme"}, "https://gl.example.com/acme/%2e%2e/evil/app.git", false},
		{"deny double-encoded traversal", []string{"https://gl.example.com/acme"}, "https://gl.example.com/acme/%252e%252e/evil/app.git", false},
		{"deny encoded-slash smuggling", []string{"https://gl.example.com/acme"}, "https://gl.example.com/acme%2f..%2fevil/app.git", false},
		{"deny dot-dot via .git trim", []string{"https://gl.example.com/acme"}, "https://gl.example.com/acme/...git", false},
		{"deny scp-style traversal", []string{"git@gl.example.com:acme"}, "git@gl.example.com:acme/../evil.git", false},
		{"deny local path traversal", []string{"/srv/git/acme"}, "/srv/git/acme/../../home/evil.git", false},

		{"userinfo must match", []string{"ssh://git@gl.example.com/acme"}, "ssh://git@gl.example.com/acme/app.git", true},
		{"deny different userinfo", []string{"ssh://git@gl.example.com/acme"}, "ssh://root@gl.example.com/acme/app.git", false},
		{"deny added userinfo", []string{"ssh://gl.example.com/acme"}, "ssh://root@gl.example.com/acme/app.git", false},
		{"git default port unified", []string{"git://gl.example.com/acme"}, "git://gl.example.com:9418/acme/app.git", true},
		{"git non-default port must match", []string{"git://gl.example.com/acme"}, "git://gl.example.com:22/acme/app.git", false},
		{"ssh default port unified", []string{"ssh://gl.example.com/acme"}, "ssh://gl.example.com:22/acme/app.git", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := mustNew(t, tc.entries...)
			if got := a.Allows(tc.url); got != tc.want {
				t.Errorf("Allows(%q) with %v = %v, want %v", tc.url, tc.entries, got, tc.want)
			}
		})
	}
}

func TestNew(t *testing.T) {
	if _, err := New([]string{"*"}); err != nil {
		t.Errorf("'*' should be accepted: %v", err)
	}
	if _, err := New([]string{"https://gl.example.com", "git@gl:acme", "/srv/git"}); err != nil {
		t.Errorf("URL/scp/path entries should be accepted: %v", err)
	}

	a, err := New([]string{" https://gl.example.com ", "", "  "})
	if err != nil {
		t.Fatalf("trim/drop-empties: %v", err)
	}
	if len(a) != 1 || a[0] != "https://gl.example.com" {
		t.Errorf("New should trim and drop empties, got %v", a)
	}

	if _, err := New([]string{"gitlab.example.com"}); err == nil {
		t.Error("bare host without scheme should be rejected")
	}
	for _, e := range []string{
		"https://gl.example.com/acme/../evil",
		"https://gl.example.com/acme/%2e%2e/evil",
		"git@gl.example.com:acme/../evil",
		"/srv/git/acme/..",
		"/",
	} {
		if _, err := New([]string{e}); err == nil {
			t.Errorf("entry %q should be rejected (matches nothing or escapes its prefix)", e)
		}
	}

	empty, err := New(nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("New(nil) = %v, %v; want empty, nil", empty, err)
	}
	if empty.Allows("https://anything/x") {
		t.Error("empty allowlist must deny")
	}
}
