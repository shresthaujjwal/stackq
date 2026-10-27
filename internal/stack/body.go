package stack

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	blockBegin = "<!-- stackq:begin -->"
	blockEnd   = "<!-- stackq:end -->"
)

// Marker is machine-readable stack metadata kept in each PR body. CI reads
// it to find a merged PR's children without any local state.
type Marker struct {
	Stack  string `json:"stack"`
	Index  int    `json:"index"`
	Total  int    `json:"total"`
	Parent string `json:"parent"`
	Trunk  string `json:"trunk"`
	Source int    `json:"source,omitempty"` // the original big PR
	// Landing is "atomic" or "incremental" ("" = incremental).
	Landing string `json:"landing,omitempty"`
	// Hold marks the activation PR, which is never queued automatically.
	Hold bool `json:"hold,omitempty"`
}

const Atomic = "atomic"

func (m Marker) Atomic() bool { return m.Landing == Atomic }

var metaRe = regexp.MustCompile(`<!-- stackq:meta (\{[^\n]*?\}) -->`)

func ParseMarker(body string) (Marker, bool) {
	m := metaRe.FindStringSubmatch(body)
	if m == nil {
		return Marker{}, false
	}
	var mk Marker
	if err := json.Unmarshal([]byte(m[1]), &mk); err != nil || mk.Stack == "" {
		return Marker{}, false
	}
	return mk, true
}

type NavEntry struct {
	Index  int
	PR     int
	Branch string
	Merged bool
	Hold   bool
	Closed bool // closed after landing (stacks landed before GitHub stacks)
}

// Lander is the PR that carries an atomic stack into the queue: the
// topmost one that isn't held back.
func Lander(nav []NavEntry) NavEntry {
	var l NavEntry
	for _, e := range nav {
		if !e.Hold && e.Index > l.Index {
			l = e
		}
	}
	return l
}

// RenderBlock renders the managed part of a PR body.
func RenderBlock(m Marker, nav []NavEntry) string {
	meta, _ := json.Marshal(m)
	sort.Slice(nav, func(i, j int) bool { return nav[i].Index < nav[j].Index })
	var b strings.Builder
	b.WriteString(blockBegin + "\n")
	fmt.Fprintf(&b, "<!-- stackq:meta %s -->\n", meta)
	fmt.Fprintf(&b, "\n---\n**Stack `%s`**: part %d of %d. ", m.Stack, m.Index, m.Total)
	lander := Lander(nav)
	switch {
	case m.Hold:
		fmt.Fprintf(&b, "🔒 **Activation PR.** It holds the changes that switch the feature on, so stackq never merges it. "+
			"It sits on top of the GitHub stack; once the rest is on `%s`, GitHub moves it there. Merge it when you are ready to enable.", m.Trunk)
	case m.Atomic():
		ref := "the top PR"
		if lander.PR > 0 {
			ref = fmt.Sprintf("#%d", lander.PR)
		}
		fmt.Fprintf(&b, "**Lands atomically:** review and approve each part, then `stackq land` asks GitHub to merge %s, "+
			"which merges the whole stack into `%s` bottom up as one request. Merging a part yourself also merges the parts below it.", ref, m.Trunk)
	default:
		fmt.Fprintf(&b, "Lands part by part, bottom up: each part is merged once it is ready, "+
			"and GitHub moves the next one onto `%s`.", m.Trunk)
	}
	if m.Source > 0 {
		fmt.Fprintf(&b, " Split from #%d.", m.Source)
	}
	b.WriteString("\n\n")
	for _, e := range nav {
		mark := "⬜"
		switch {
		case e.Merged, e.Closed:
			mark = "✅"
		case e.Hold:
			mark = "🔒"
		}
		ref := "(no PR yet)"
		if e.PR > 0 {
			ref = fmt.Sprintf("#%d", e.PR)
		}
		line := fmt.Sprintf("%d. %s %s `%s`", e.Index, mark, ref, e.Branch)
		if m.Atomic() && !e.Hold && e.Index == lander.Index {
			line += " (lander)"
		}
		if e.Index == m.Index {
			line += " 👈 this PR"
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(blockEnd)
	return b.String()
}

// MergeBody replaces the managed block in an existing body, keeping
// whatever the author wrote around it.
func MergeBody(existing, block string) string {
	i := strings.Index(existing, blockBegin)
	j := strings.Index(existing, blockEnd)
	if i >= 0 && j > i {
		return existing[:i] + block + existing[j+len(blockEnd):]
	}
	if strings.TrimSpace(existing) == "" {
		return block
	}
	return strings.TrimRight(existing, "\n") + "\n\n" + block
}

const (
	statusBegin = "<!-- stackq:status:begin -->"
	statusEnd   = "<!-- stackq:status:end -->"
)

// MergeStatus puts status in its own managed section of a PR body, after the
// stack block, replacing any earlier status. An empty status removes the
// section. The stack block is rewritten on every restack, so the status lives
// outside it.
func MergeStatus(body, status string) string {
	i := strings.Index(body, statusBegin)
	j := strings.Index(body, statusEnd)
	if i >= 0 && j > i {
		body = strings.TrimRight(body[:i], "\n") + body[j+len(statusEnd):]
	}
	if status == "" {
		return body
	}
	section := statusBegin + "\n" + strings.TrimSpace(status) + "\n" + statusEnd
	if k := strings.Index(body, blockEnd); k >= 0 {
		k += len(blockEnd)
		return body[:k] + "\n\n" + section + body[k:]
	}
	if strings.TrimSpace(body) == "" {
		return section
	}
	return strings.TrimRight(body, "\n") + "\n\n" + section
}
