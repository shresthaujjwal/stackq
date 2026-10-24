package prio

import (
	"testing"
	"time"
)

func cfg(t *testing.T) Config {
	t.Helper()
	c := Defaults()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClassify(t *testing.T) {
	c := cfg(t)
	big := 900
	cases := []struct {
		name string
		pr   PR
		want string
	}{
		{"cve in title", PR{Title: "Bump openssl to fix CVE-2026-12345", Lines: big, Files: 40}, "urgent"},
		{"ghsa in body", PR{Title: "Bump x", Body: "Fixes GHSA-abcd-1234-wxyz", Lines: big}, "urgent"},
		{"security label", PR{Title: "patch", Labels: []string{"Security"}, Lines: big}, "urgent"},
		{"hotfix branch", PR{Title: "fix", Branch: "hotfix/login", Lines: big}, "urgent"},
		{"revert", PR{Title: "Revert stack checkout (#12)", Lines: big}, "urgent"},
		{"dependabot", PR{Title: "Bump lodash from 4.17.20 to 4.17.21", Author: "dependabot[bot]", Branch: "dependabot/npm_and_yarn/lodash", Lines: 30}, "bots"},
		{"automerge label", PR{Title: "tidy", Labels: []string{"automerge"}, Lines: big}, "bots"},
		{"small", PR{Title: "typo", Lines: 4, Files: 1}, "small"},
		{"small but many files", PR{Title: "rename", Lines: 20, Files: 12}, "normal"},
		{"big", PR{Title: "new checkout", Lines: big, Files: 30}, "normal"},
		{"cve-like but not one", PR{Title: "CVE-12 notes", Lines: big}, "normal"},
	}
	for _, tc := range cases {
		got, why := c.Classify(tc.pr)
		if got.Name != tc.want {
			t.Errorf("%s: got %s (%s), want %s", tc.name, got.Name, why, tc.want)
		}
	}
}

func TestRankAgingPreventsStarvation(t *testing.T) {
	c := cfg(t)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	pool := []PR{
		{Number: 1, Title: "big feature", Lines: 800, Files: 20, ReadyAt: now.Add(-15 * time.Hour), Eligible: true},
		{Number: 2, Title: "typo", Lines: 2, Files: 1, ReadyAt: now.Add(-1 * time.Minute), Eligible: true},
		{Number: 3, Title: "another big one", Lines: 800, Files: 20, ReadyAt: now.Add(-10 * time.Minute), Eligible: true},
	}
	r := c.Rank(pool, now)
	// 15h waiting = 30 aging points > small's 20 weight.
	if r[0].PR.Number != 1 || r[1].PR.Number != 2 || r[2].PR.Number != 3 {
		t.Fatalf("order = %d,%d,%d", r[0].PR.Number, r[1].PR.Number, r[2].PR.Number)
	}
	// Fresh, the small PR goes first.
	pool[0].ReadyAt = now.Add(-10 * time.Minute)
	r = c.Rank(pool, now)
	if r[0].PR.Number != 2 {
		t.Fatalf("small PR should lead, got #%d", r[0].PR.Number)
	}
}

func TestDecideJumpBudgetAndSlots(t *testing.T) {
	c := cfg(t) // MaxInFlight 3, MaxJumpsInFlight 1
	now := time.Now()
	pool := []PR{
		{Number: 10, Title: "big", Lines: 900, Files: 30, ReadyAt: now.Add(-time.Hour), Eligible: true},
		{Number: 11, Title: "fix CVE-2026-0001", Lines: 900, ReadyAt: now, Eligible: true},
		{Number: 12, Title: "fix CVE-2026-0002", Lines: 900, ReadyAt: now, Eligible: true},
		{Number: 13, Title: "typo", Lines: 1, Files: 1, ReadyAt: now, Eligible: true},
		{Number: 14, Title: "typo 2", Lines: 1, Files: 1, ReadyAt: now, Eligible: false, Why: "checks running"},
	}
	ds := c.Decide(c.Rank(pool, now), 1, 0)
	got := map[int]Action{}
	order := []int{}
	for _, d := range ds {
		got[d.PR.Number] = d.Action
		order = append(order, d.PR.Number)
	}
	if order[0] != 11 || order[1] != 12 || order[2] != 13 {
		t.Fatalf("ranking: %v", order)
	}
	// 1 already in flight: #11 jumps (2), #12 has no jump budget so it is
	// admitted normally (3), the queue is then full.
	want := map[int]Action{11: Jump, 12: Admit, 13: Wait, 10: Wait, 14: Skip}
	for n, a := range want {
		if got[n] != a {
			t.Errorf("#%d: got %s, want %s", n, got[n], a)
		}
	}
}

func TestCustomClassesSizeAndSignalTogether(t *testing.T) {
	c := Config{Classes: []Class{
		{Name: "small-bot", Weight: 50, Match: Match{Authors: []string{"renovate[bot]"}, MaxLines: 20}},
		{Name: "normal"},
	}}.WithDefaults()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if cl, _ := c.Classify(PR{Author: "renovate[bot]", Lines: 10}); cl.Name != "small-bot" {
		t.Fatal("small bot PR should match")
	}
	if cl, _ := c.Classify(PR{Author: "renovate[bot]", Lines: 400}); cl.Name != "normal" {
		t.Fatal("size limit must also hold")
	}
}
