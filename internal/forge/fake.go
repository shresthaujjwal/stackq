package forge

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Fake is an in-memory Forge for tests.
type Fake struct {
	mu       sync.Mutex
	PRs      map[int]*PR
	next     int
	Comments map[int][]string
	Labels   map[int]map[string]bool
	Enqueued []int
	// MQ is the simulated merge queue, front first.
	MQ []QueueEntry
	// Jumped records PRs enqueued with Jump.
	Jumped []int
	// Ready overrides ReadyAt per PR; default is the PR's CreatedAt.
	Ready map[int]time.Time
	// PRDetails overrides Details per PR; the default is one approval and
	// passing checks.
	PRDetails  map[int]Details
	EnqueueErr error
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
		PRDetails: map[int]Details{}, Ready: map[int]time.Time{}}
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
		if state == "all" || p.Open() {
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
		HeadRef: head, BaseRef: base, State: "OPEN"}
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
	if base != nil {
		p.BaseRef = *base
	}
	if body != nil {
		p.Body = *body
	}
	return nil
}

func (f *Fake) Enqueue(pr PR, o EnqueueOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.EnqueueErr != nil {
		return f.EnqueueErr
	}
	f.Enqueued = append(f.Enqueued, pr.Number)
	e := QueueEntry{PR: pr.Number, State: "QUEUED", Jumped: o.Jump}
	if o.Jump {
		f.Jumped = append(f.Jumped, pr.Number)
		f.MQ = append([]QueueEntry{e}, f.MQ...)
	} else {
		f.MQ = append(f.MQ, e)
	}
	for i := range f.MQ {
		f.MQ[i].Position = i + 1
	}
	return nil
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
func (f *Fake) MarkMerged(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.PRs[n]; ok {
		f.refresh(p)
		p.State = "MERGED"
	}
}
