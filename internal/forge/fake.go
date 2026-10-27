package forge

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Fake is an in-memory Forge for tests.
// ReadyKey picks one PR and label for Fake.ReadyFor.
type ReadyKey struct {
	PR    int
	Label string
}

type Fake struct {
	mu       sync.Mutex
	PRs      map[int]*PR
	next     int
	Comments map[int][]string
	Labels   map[int]map[string]bool
	// Enqueued is every PR a Merge or Jump put in the queue, in order.
	Enqueued []int
	// Merges is the PR each Merge call was made for, in order.
	Merges []int
	// MQ is the simulated merge queue, front first.
	MQ []QueueEntry
	// Jumped records PRs queued with Jump.
	Jumped []int
	// Stacks are the GitHub stacks, bottom first, by stack number; Base is
	// each stack's trunk.
	Stacks    map[int][]int
	StackBase map[int]string
	// MergePending leaves merge requests pending instead of enqueued, as
	// GitHub does for the first moments.
	MergePending bool
	// Requests are the merge requests by uuid; tests may change their status.
	Requests map[string]*MergeRequest
	// Ready overrides ReadyAt per PR; default is the PR's CreatedAt.
	Ready map[int]time.Time
	// ReadyFor overrides ReadyAt for one PR and label.
	ReadyFor map[ReadyKey]time.Time
	// PRDetails overrides Details per PR; the default is one approval and
	// passing checks.
	PRDetails map[int]Details
	// MergeErr makes Merge and Jump fail.
	MergeErr error
	// QueueErr makes Queue fail, as when the branch has no merge queue.
	QueueErr error
	// PoolLag makes ListPool return nothing, as when the label search has not
	// indexed a just-added label yet.
	PoolLag bool
	// HeadSHA resolves a branch to its current tip so open PRs track pushes.
	HeadSHA func(branch string) string
}

func NewFake() *Fake {
	return &Fake{PRs: map[int]*PR{}, next: 1, Comments: map[int][]string{}, Labels: map[int]map[string]bool{},
		PRDetails: map[int]Details{}, Ready: map[int]time.Time{}, Stacks: map[int][]int{}, StackBase: map[int]string{},
		Requests: map[string]*MergeRequest{}}
}

func (f *Fake) refresh(p *PR) {
	if p.Open() && f.HeadSHA != nil {
		if sha := f.HeadSHA(p.HeadRef); sha != "" {
			p.HeadSHA = sha
		}
	}
}

func (f *Fake) ListPRs(state string) ([]PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []PR
	for _, p := range f.PRs {
		f.refresh(p)
		if state == "all" || p.Open() || state == "closed" && !p.Open() {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

func (f *Fake) GetPR(n int) (PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.PRs[n]
	if !ok {
		return PR{}, fmt.Errorf("no PR #%d", n)
	}
	f.refresh(p)
	return *p, nil
}

func (f *Fake) Details(n int) (Details, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.PRs[n]; !ok {
		return Details{}, fmt.Errorf("no PR #%d", n)
	}
	if d, ok := f.PRDetails[n]; ok {
		return d, nil
	}
	return Details{Approvals: []string{"reviewer"}, Checks: "success"}, nil
}

// SetDetails replaces the details of PR n, keeping fields left empty.
func (f *Fake) SetDetails(n int, fn func(*Details)) {
	f.mu.Lock()
	d, ok := f.PRDetails[n]
	if !ok {
		d = Details{Approvals: []string{"reviewer"}, Checks: "success"}
	}
	fn(&d)
	f.PRDetails[n] = d
	f.mu.Unlock()
}

func (f *Fake) FindOpenPRByHead(head string) (*PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.PRs {
		if p.Open() && p.HeadRef == head {
			f.refresh(p)
			c := *p
			return &c, nil
		}
	}
	return nil, nil
}

func (f *Fake) CreatePR(head, base, title, body string, draft bool) (PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := &PR{Number: f.next, NodeID: fmt.Sprintf("PR_%d", f.next), Title: title, Body: body,
		HeadRef: head, BaseRef: base, State: "OPEN", Draft: draft}
	f.next++
	f.refresh(p)
	f.PRs[p.Number] = p
	return *p, nil
}

func (f *Fake) EditPR(n int, base, body *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.PRs[n]
	if !ok {
		return fmt.Errorf("no PR #%d", n)
	}
	if base != nil && *base != p.BaseRef {
		if f.stackOf(n) != 0 {
			return fmt.Errorf("Validation Failed: Cannot change the base branch because the pull request is part of a stack.")
		}
		p.BaseRef = *base
	}
	if body != nil {
		p.Body = *body
	}
	return nil
}

func (f *Fake) stackOf(n int) int {
	for num, prs := range f.Stacks {
		for _, p := range prs {
			if p == n {
				return num
			}
		}
	}
	return 0
}

func (f *Fake) queue(n int, jump bool) {
	f.Enqueued = append(f.Enqueued, n)
	e := QueueEntry{PR: n, State: "QUEUED", Jumped: jump}
	if jump {
		f.Jumped = append(f.Jumped, n)
		f.MQ = append([]QueueEntry{e}, f.MQ...)
	} else {
		f.MQ = append(f.MQ, e)
	}
	for i := range f.MQ {
		f.MQ[i].Position = i + 1
	}
}

// Merge queues PR n and, if it is stacked, every open PR below it, as one
// request. A draft or closed PR, or a moved head, fails the request.
func (f *Fake) Merge(n int, headSHA string) (MergeRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.MergeErr != nil {
		return MergeRequest{}, f.MergeErr
	}
	p, ok := f.PRs[n]
	if !ok {
		return MergeRequest{}, fmt.Errorf("no PR #%d", n)
	}
	f.refresh(p)
	f.Merges = append(f.Merges, n)
	uuid := fmt.Sprintf("uuid-%d-%d", n, len(f.Merges))
	fail := func(msg string) (MergeRequest, error) {
		r := &MergeRequest{UUID: uuid, Status: MergeFailed, Message: msg}
		f.Requests[uuid] = r
		return *r, nil
	}
	switch {
	case p.Merged():
		return MergeRequest{Status: MergeMerged, Message: "Pull request is already merged."}, nil
	case !p.Open():
		return fail("Pull request is closed")
	case p.Draft:
		return fail("Pull request is in draft")
	case headSHA != "" && p.HeadSHA != "" && headSHA != p.HeadSHA:
		return fail("expected head sha does not match")
	}
	set := []int{n}
	if s := f.stackOf(n); s != 0 {
		set = nil
		for _, m := range f.Stacks[s] {
			q := f.PRs[m]
			if q.Open() {
				if q.Draft {
					return fail(fmt.Sprintf("#%d below it is a draft", m))
				}
				set = append(set, m)
			}
			if m == n {
				break
			}
		}
	}
	for _, m := range set {
		f.queue(m, false)
	}
	status := MergeEnqueued
	if f.MergePending {
		status = MergePending
	}
	r := &MergeRequest{UUID: uuid, Status: status, Message: "Merge request enqueued."}
	f.Requests[uuid] = r
	return *r, nil
}

func (f *Fake) MergeResult(n int, uuid string) (MergeRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.Requests[uuid]
	if !ok {
		return MergeRequest{}, fmt.Errorf("no merge request %s for #%d", uuid, n)
	}
	return *r, nil
}

func (f *Fake) Jump(pr PR) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.MergeErr != nil {
		return f.MergeErr
	}
	if f.stackOf(pr.Number) != 0 {
		return fmt.Errorf("#%d is in a stack, which cannot jump", pr.Number)
	}
	f.queue(pr.Number, true)
	return nil
}

func (f *Fake) StackOf(n int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stackOf(n), nil
}

func (f *Fake) StackPRs(s int) ([]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prs, ok := f.Stacks[s]
	if !ok {
		return nil, fmt.Errorf("no stack %d", s)
	}
	return append([]int(nil), prs...), nil
}

// chain checks GitHub's rule: each PR targets the head branch of the one before.
func (f *Fake) chain(below string, prs []int) error {
	for _, n := range prs {
		p, ok := f.PRs[n]
		if !ok || !p.Open() {
			return fmt.Errorf("Validation Failed: #%d is not an open pull request", n)
		}
		if f.stackOf(n) != 0 {
			return fmt.Errorf("Validation Failed: #%d is already in a stack", n)
		}
		if below != "" && p.BaseRef != below {
			return fmt.Errorf("Validation Failed: #%d targets %s, not %s", n, p.BaseRef, below)
		}
		below = p.HeadRef
	}
	return nil
}

func (f *Fake) CreateStack(prs []int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(prs) < 2 {
		return 0, fmt.Errorf("Validation Failed: a stack needs at least 2 pull requests")
	}
	if err := f.chain("", prs); err != nil {
		return 0, err
	}
	n := f.next
	f.next++
	f.Stacks[n] = append([]int(nil), prs...)
	f.StackBase[n] = f.PRs[prs[0]].BaseRef
	return n, nil
}

func (f *Fake) AddToStack(s int, prs []int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.Stacks[s]
	if !ok {
		return fmt.Errorf("no stack %d", s)
	}
	if err := f.chain(f.PRs[cur[len(cur)-1]].HeadRef, prs); err != nil {
		return err
	}
	f.Stacks[s] = append(cur, prs...)
	return nil
}

// LandQueue merges everything in the simulated queue, front first, the way
// the merge queue does when its checks pass. Like GitHub, it then retargets
// the open PRs of a stack whose base branch was merged onto the stack's base.
func (f *Fake) LandQueue() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var merged []int
	gone := map[string]bool{}
	for _, e := range f.MQ {
		if p := f.PRs[e.PR]; p != nil && p.Open() {
			f.refresh(p)
			p.State = "MERGED"
			gone[p.HeadRef] = true
			merged = append(merged, e.PR)
		}
	}
	f.MQ = nil
	for s, prs := range f.Stacks {
		for _, n := range prs {
			if p := f.PRs[n]; p.Open() && gone[p.BaseRef] {
				p.BaseRef = f.StackBase[s]
			}
		}
	}
	return merged
}

func (f *Fake) ListPool(label string) ([]PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.PoolLag {
		return nil, nil
	}
	var out []PR
	for n, p := range f.PRs {
		if !p.Open() || !f.Labels[n][label] {
			continue
		}
		f.refresh(p)
		c := *p
		c.Labels = nil
		for l := range f.Labels[n] {
			c.Labels = append(c.Labels, l)
		}
		sort.Strings(c.Labels)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

func (f *Fake) Queue(branch string) ([]QueueEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.QueueErr != nil {
		return nil, f.QueueErr
	}
	return append([]QueueEntry{}, f.MQ...), nil
}

func (f *Fake) ReadyAt(n int, label string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.ReadyFor[ReadyKey{n, label}]; ok {
		return t, nil
	}
	if t, ok := f.Ready[n]; ok {
		return t, nil
	}
	if p, ok := f.PRs[n]; ok {
		return p.CreatedAt, nil
	}
	return time.Time{}, nil
}

// Dequeue removes PR n from the simulated queue (merged or ejected).
func (f *Fake) Dequeue(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var q []QueueEntry
	for _, e := range f.MQ {
		if e.PR != n {
			q = append(q, e)
		}
	}
	for i := range q {
		q[i].Position = i + 1
	}
	f.MQ = q
}

func (f *Fake) Comment(n int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Comments[n] = append(f.Comments[n], body)
	return nil
}

func (f *Fake) ClosePR(n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.PRs[n]
	if !ok {
		return fmt.Errorf("no PR #%d", n)
	}
	if p.State == "OPEN" {
		p.State = "CLOSED"
	}
	return nil
}

func (f *Fake) AddLabels(n int, labels ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Labels[n] == nil {
		f.Labels[n] = map[string]bool{}
	}
	for _, l := range labels {
		f.Labels[n][l] = true
	}
	return nil
}

func (f *Fake) RemoveLabels(n int, labels ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range labels {
		delete(f.Labels[n], l)
	}
	return nil
}

// MarkMerged records that the queue merged PR n, freezing its head SHA.
// MarkMerged records PR n as merged. If it is in a stack, the open PR that
// targeted its branch is retargeted onto the stack's base, as GitHub does.
func (f *Fake) MarkMerged(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.PRs[n]
	if !ok {
		return
	}
	f.refresh(p)
	p.State = "MERGED"
	if st := f.stackOf(n); st != 0 {
		for _, m := range f.Stacks[st] {
			if q := f.PRs[m]; q.Open() && q.BaseRef == p.HeadRef {
				q.BaseRef = f.StackBase[st]
			}
		}
	}
}
