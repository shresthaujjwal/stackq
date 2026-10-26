// Package prio decides which waiting PR enters the merge queue next.
//
// GitHub's merge queue is first-in, first-out; its only priority lever is
// "jump to the front", which rebuilds every in-flight entry. stackq
// therefore does admission control: PRs wait in a pool (a label), GitHub's
// queue is kept short (MaxInFlight), and whenever there is room the
// highest-scoring ready PR is enqueued. Urgent classes may also jump, within
// a small budget so jumps can't turn into a rebuild storm.
package prio

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"stackq/internal/plural"
)

// Match selects PRs for a class. Signal fields are OR'd: any one matching
// is enough. Size limits, when set, must also hold (AND).
type Match struct {
	Labels      []string `json:"labels,omitempty"`
	Authors     []string `json:"authors,omitempty"`
	Branches    []string `json:"branches,omitempty"` // globs on the head branch, e.g. "dependabot/*"
	TitleRegex  string   `json:"titleRegex,omitempty"`
	BodyRegex   string   `json:"bodyRegex,omitempty"`
	Reverts     bool     `json:"reverts,omitempty"` // titles starting with "Revert"
	MaxLines    int      `json:"maxLines,omitempty"`
	MaxFiles    int      `json:"maxFiles,omitempty"`
	titleRe     *regexp.Regexp
	bodyRe      *regexp.Regexp
	branchRes   []*regexp.Regexp
	hasSignals  bool
	initialized bool
}

type Class struct {
	Name   string `json:"name"`
	Weight int    `json:"weight"`
	// Jump lets this class go to the front of GitHub's queue.
	Jump  bool  `json:"jump,omitempty"`
	Match Match `json:"match"`
}

type Config struct {
	Enabled bool `json:"enabled"`
	// Label marks PRs waiting for stackq to queue them.
	Label string `json:"label,omitempty"`
	// MaxInFlight caps entries in GitHub's queue that stackq tops up to.
	// Set it to at least the queue's maximum group size.
	MaxInFlight int `json:"maxInFlight,omitempty"`
	// AgingMinutes adds one point of score per this many minutes waited,
	// so big PRs can't starve. 0 disables aging.
	AgingMinutes int `json:"agingMinutes,omitempty"`
	// MaxJumpsInFlight caps jumped entries in the queue at once.
	MaxJumpsInFlight int     `json:"maxJumpsInFlight,omitempty"`
	Classes          []Class `json:"classes,omitempty"`
	// ApprovalRequired keeps unapproved PRs in the pool (default true).
	ApprovalRequired *bool `json:"requireApproval,omitempty"`
}

func (c *Config) RequireApproval() bool { return c.ApprovalRequired == nil || *c.ApprovalRequired }

// Defaults: vuln fixes and reverts jump; bot/automerge and small PRs go
// ahead of the rest; everything ages.
func Defaults() Config {
	return Config{
		Label:            "stackq:queue",
		MaxInFlight:      3,
		AgingMinutes:     30,
		MaxJumpsInFlight: 1,
		Classes: []Class{
			{Name: "urgent", Weight: 100, Jump: true, Match: Match{
				Labels:     []string{"security", "vulnerability", "hotfix", "stackq:revert"},
				Branches:   []string{"hotfix/*", "security/*"},
				TitleRegex: `(?i)\b(CVE-\d{4}-\d{4,}|GHSA(-[0-9a-z]{4}){3})\b|\bsecurity fix\b`,
				BodyRegex:  `(?i)\b(CVE-\d{4}-\d{4,}|GHSA(-[0-9a-z]{4}){3})\b`,
				Reverts:    true,
			}},
			{Name: "bots", Weight: 30, Match: Match{
				Authors:  []string{"dependabot[bot]", "renovate[bot]", "app/dependabot", "app/renovate"},
				Labels:   []string{"automerge", "dependencies"},
				Branches: []string{"dependabot/*", "renovate/*"},
			}},
			{Name: "small", Weight: 20, Match: Match{MaxLines: 50, MaxFiles: 5}},
			{Name: "normal", Weight: 0},
		},
	}
}

// WithDefaults fills unset fields from Defaults.
func (c Config) WithDefaults() Config {
	d := Defaults()
	if c.Label == "" {
		c.Label = d.Label
	}
	if c.MaxInFlight <= 0 {
		c.MaxInFlight = d.MaxInFlight
	}
	if c.AgingMinutes < 0 {
		c.AgingMinutes = 0
	} else if c.AgingMinutes == 0 {
		c.AgingMinutes = d.AgingMinutes
	}
	if c.MaxJumpsInFlight < 0 {
		c.MaxJumpsInFlight = 0
	} else if c.MaxJumpsInFlight == 0 {
		c.MaxJumpsInFlight = d.MaxJumpsInFlight
	}
	if len(c.Classes) == 0 {
		c.Classes = d.Classes
	}
	return c
}

// PR is what classification looks at.
type PR struct {
	Number   int
	Title    string
	Body     string
	Branch   string
	Author   string
	Labels   []string
	Lines    int // additions + deletions
	Files    int
	ReadyAt  time.Time // when it entered the pool
	Eligible bool
	Why      string // why it isn't eligible
}

func (m *Match) init() error {
	if m.initialized {
		return nil
	}
	var err error
	if m.TitleRegex != "" {
		if m.titleRe, err = regexp.Compile(m.TitleRegex); err != nil {
			return fmt.Errorf("titleRegex: %w", err)
		}
	}
	if m.BodyRegex != "" {
		if m.bodyRe, err = regexp.Compile(m.BodyRegex); err != nil {
			return fmt.Errorf("bodyRegex: %w", err)
		}
	}
	for _, b := range m.Branches {
		re, err := regexp.Compile("^" + strings.ReplaceAll(regexp.QuoteMeta(b), `\*`, ".*") + "$")
		if err != nil {
			return err
		}
		m.branchRes = append(m.branchRes, re)
	}
	m.hasSignals = len(m.Labels)+len(m.Authors)+len(m.Branches) > 0 || m.TitleRegex != "" || m.BodyRegex != "" || m.Reverts
	m.initialized = true
	return nil
}

// Validate compiles every class's patterns.
func (c *Config) Validate() error {
	for i := range c.Classes {
		if err := c.Classes[i].Match.init(); err != nil {
			return fmt.Errorf("priority class %q: %w", c.Classes[i].Name, err)
		}
	}
	return nil
}

// matches returns whether p matches and the signal that matched.
func (m *Match) matches(p PR) (bool, string) {
	if m.MaxLines > 0 && p.Lines > m.MaxLines {
		return false, ""
	}
	if m.MaxFiles > 0 && p.Files > m.MaxFiles {
		return false, ""
	}
	if !m.hasSignals {
		if m.MaxLines > 0 || m.MaxFiles > 0 {
			return true, fmt.Sprintf("%s in %s", plural.N(p.Lines, "line"), plural.N(p.Files, "file"))
		}
		return true, "default"
	}
	for _, want := range m.Labels {
		for _, l := range p.Labels {
			if strings.EqualFold(l, want) {
				return true, "label " + l
			}
		}
	}
	for _, a := range m.Authors {
		if strings.EqualFold(p.Author, a) {
			return true, "author " + p.Author
		}
	}
	for _, re := range m.branchRes {
		if re.MatchString(p.Branch) {
			return true, "branch " + p.Branch
		}
	}
	if m.Reverts && strings.HasPrefix(p.Title, "Revert") {
		return true, "revert"
	}
	if m.titleRe != nil {
		if s := m.titleRe.FindString(p.Title); s != "" {
			return true, "title " + s
		}
	}
	if m.bodyRe != nil {
		if s := m.bodyRe.FindString(p.Body); s != "" {
			return true, "body " + s
		}
	}
	return false, ""
}

// Classify returns the first matching class (classes are in priority
// order) and the reason. A PR that matches nothing gets a zero class.
func (c *Config) Classify(p PR) (Class, string) {
	for i := range c.Classes {
		c.Classes[i].Match.init() // errors surface in Validate
	}
	for _, cl := range c.Classes {
		if ok, why := cl.Match.matches(p); ok {
			return cl, why
		}
	}
	return Class{Name: "normal"}, "default"
}

// Ranked is a pool entry with its class and score.
type Ranked struct {
	PR     PR
	Class  Class
	Reason string
	Waited time.Duration
	Score  float64
}

// Rank scores and orders the pool, best first. Ties go to whoever waited
// longest, then the lower PR number.
func (c *Config) Rank(pool []PR, now time.Time) []Ranked {
	var out []Ranked
	for _, p := range pool {
		cl, why := c.Classify(p)
		waited := now.Sub(p.ReadyAt)
		if waited < 0 || p.ReadyAt.IsZero() {
			waited = 0
		}
		score := float64(cl.Weight)
		if c.AgingMinutes > 0 {
			score += waited.Minutes() / float64(c.AgingMinutes)
		}
		out = append(out, Ranked{PR: p, Class: cl, Reason: why, Waited: waited, Score: score})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Waited != b.Waited {
			return a.Waited > b.Waited
		}
		return a.PR.Number < b.PR.Number
	})
	return out
}

// Action is what the scheduler does with one ranked PR.
type Action string

const (
	Jump  Action = "jump"  // enqueue at the front
	Admit Action = "admit" // enqueue at the back
	Wait  Action = "wait"  // stay in the pool
	Skip  Action = "skip"  // not eligible yet
)

type Decision struct {
	Ranked
	Action Action
	Note   string
}

// Decide plans one scheduling pass. inFlight is the current length of
// GitHub's queue and jumpsInFlight how many of those entries jumped.
func (c *Config) Decide(ranked []Ranked, inFlight, jumpsInFlight int) []Decision {
	var out []Decision
	for _, r := range ranked {
		d := Decision{Ranked: r}
		switch {
		case !r.PR.Eligible:
			d.Action, d.Note = Skip, r.PR.Why
		case r.Class.Jump && jumpsInFlight < c.MaxJumpsInFlight:
			d.Action, d.Note = Jump, "urgent: front of the queue"
			jumpsInFlight++
			inFlight++
		case inFlight < c.MaxInFlight:
			d.Action = Admit
			if r.Class.Jump {
				d.Note = "jump budget used; first in line instead"
			}
			inFlight++
		default:
			d.Action, d.Note = Wait, fmt.Sprintf("queue full (%d/%d)", inFlight, c.MaxInFlight)
		}
		out = append(out, d)
	}
	return out
}
