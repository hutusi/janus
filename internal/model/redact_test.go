package model

import "testing"

func TestRedactURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://user:token@gitlab.example.com/acme/app.git", "https://gitlab.example.com/acme/app.git"},
		{"https://oauth2@gitlab.example.com/acme/app.git", "https://gitlab.example.com/acme/app.git"},
		{"https://gitlab.example.com/acme/app.git", "https://gitlab.example.com/acme/app.git"},
		{"ssh://git@gitlab.example.com/acme/app.git", "ssh://gitlab.example.com/acme/app.git"},
		{"git@gitlab.example.com:acme/app.git", "git@gitlab.example.com:acme/app.git"}, // scp-style: not a secret
		{"://not a url", "://not a url"},
		{"/local/path", "/local/path"},
		{"", ""},
		// URLs embedded in longer text — git errors echo the invoked URL.
		{
			"git remote set-url origin https://u:p@host/a.git: exit status 128",
			"git remote set-url origin https://host/a.git: exit status 128",
		},
		{
			"fetch https://a@h/one failed, fetch https://b:c@h/two failed",
			"fetch https://h/one failed, fetch https://h/two failed",
		},
	}
	for _, tc := range cases {
		if got := RedactURL(tc.in); got != tc.want {
			t.Errorf("RedactURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
