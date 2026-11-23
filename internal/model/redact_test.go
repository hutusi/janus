package model

import "testing"

func TestRedactURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://user:token@gitlab.example.com/acme/app.git", "https://gitlab.example.com/acme/app.git"},
		{"https://oauth2@gitlab.example.com/acme/app.git", "https://gitlab.example.com/acme/app.git"}, // username slot can hold a token
		{"https://gitlab.example.com/acme/app.git", "https://gitlab.example.com/acme/app.git"},
		{"http://ghp_secret@host/a.git", "http://host/a.git"},
		// SSH usernames are the account git connects as, not a credential.
		{"ssh://git@gitlab.example.com/acme/app.git", "ssh://git@gitlab.example.com/acme/app.git"},
		{"ssh://git@host:2222/group/app.git", "ssh://git@host:2222/group/app.git"},
		{"SSH://git@host/a.git", "SSH://git@host/a.git"}, // schemes are case-insensitive
		{"git+ssh://git@host/a.git", "git+ssh://git@host/a.git"},
		{"ssh+git://git@host/a.git", "ssh+git://git@host/a.git"},
		// ...but a password on an SSH URL is still a secret: drop the whole userinfo.
		{"ssh://git:hunter2@host/a.git", "ssh://host/a.git"},
		// A raw "@" inside a password: the userinfo runs to the LAST "@", so no
		// tail of the password survives, on one pass or two.
		{"ssh://git:pa@ss@host/a.git", "ssh://host/a.git"},
		{"https://u:pa@ss@host/a.git", "https://host/a.git"},
		{"ssh://a@b@host:2222/x.git", "ssh://host:2222/x.git"}, // not one plain username
		// "@" after the authority is not userinfo: path, query, fragment.
		{"https://host/a@b", "https://host/a@b"},
		{"https://host?x=a@b", "https://host?x=a@b"},
		{"https://host#a@b", "https://host#a@b"},
		{"https://u:p@host/team@corp/repo.git", "https://host/team@corp/repo.git"},
		{"ssh://host/a.git", "ssh://host/a.git"},
		// A scheme that merely contains "ssh" is not an SSH scheme.
		{"sshx://user@host/a.git", "sshx://host/a.git"},
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
		{
			"git clone ssh://git@h:2222/a.git: exit status 128, then https://t@h/a.git",
			"git clone ssh://git@h:2222/a.git: exit status 128, then https://h/a.git",
		},
		// The greedy userinfo stops at whitespace, so an address later in
		// the message is not swallowed into a URL's authority.
		{
			"clone https://h failed; contact ops@example.com",
			"clone https://h failed; contact ops@example.com",
		},
	}
	for _, tc := range cases {
		if got := RedactURL(tc.in); got != tc.want {
			t.Errorf("RedactURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
