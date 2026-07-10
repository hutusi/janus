package model

import "testing"

func TestBranchFilterMatches(t *testing.T) {
	tests := []struct {
		name   string
		filter BranchFilter
		branch string
		want   bool
	}{
		{"empty filter matches all", BranchFilter{}, "anything", true},
		{"allowlist hit", BranchFilter{Branches: []string{"main"}}, "main", true},
		{"allowlist miss", BranchFilter{Branches: []string{"main"}}, "dev", false},
		{"denylist hit", BranchFilter{Ignore: []string{"master"}}, "master", false},
		{"denylist miss matches all", BranchFilter{Ignore: []string{"master"}}, "feature/x", true},
		// Both lists is rejected by pipeline validation, but hand-built
		// workflows can carry it — deny must win.
		{"deny wins over allow", BranchFilter{Branches: []string{"main"}, Ignore: []string{"main"}}, "main", false},
		{"present-but-empty denylist matches all", BranchFilter{Ignore: []string{}}, "anything", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.filter.Matches(tc.branch); got != tc.want {
				t.Errorf("Matches(%q) = %v, want %v", tc.branch, got, tc.want)
			}
		})
	}
}
