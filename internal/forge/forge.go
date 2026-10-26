// Package forge is the GitHub side of stackq: pull requests, comments,
// labels and the merge queue.
package forge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

type PR struct {
	Number  int
	NodeID  string
	Title   string
	Body    string
	HeadRef string
	BaseRef string
	HeadSHA string
	State   string // OPEN, CLOSED or MERGED
	// Filled by ListPool (and the fake) for prioritization.
	Author       string
	Labels       []string
	Additions    int
	Deletions    int
	ChangedFiles int
	Draft        bool
	CreatedAt    time.Time
	// CrossRepo is true when the head branch lives in a fork. Anyone can open
	// such a PR with any body, so stackq never treats one as part of a stack.
	CrossRepo bool
}

// QueueEntry is one PR in GitHub's merge queue.
type QueueEntry struct {
	PR       int
	Position int
	State    string // QUEUED, AWAITING_CHECKS, MERGEABLE, UNMERGEABLE, LOCKED
	Jumped   bool   // labelled stackq:jumped
}

type EnqueueOptions struct {
	// Jump puts the PR at the front of the queue. GitHub rebuilds every
	// in-flight entry when this happens.
	Jump bool
}

// Details is the review, check and merge state of a PR, fetched on demand.
type Details struct {
	Approvals        []string // logins whose latest review approves
	ChangesRequested []string // logins whose latest review requests changes
	Checks           string   // success, pending, failure or none
	MergeCommit      string   // the commit GitHub recorded on the base branch
	AutoMerge        bool     // auto-merge is on and waiting for the PR's requirements
	Commits          int      // commits in the PR
}

func (p PR) Merged() bool { return p.State == "MERGED" }
func (p PR) Open() bool   { return p.State == "OPEN" }

// Forge is everything stackq needs from the code host.
type Forge interface {
	// ListPRs returns open PRs, plus recently merged ones when state is "all".
	// "closed" returns recently closed PRs, merged or not.
	ListPRs(state string) ([]PR, error)
	GetPR(n int) (PR, error)
	Details(n int) (Details, error)
	FindOpenPRByHead(head string) (*PR, error)
	CreatePR(head, base, title, body string, draft bool) (PR, error)
	// EditPR changes the base and/or body; nil leaves a field alone.
	EditPR(n int, base, body *string) error
	// Enqueue adds the PR to the merge queue, or arranges for it to join the
	// queue as soon as its required checks pass.
	Enqueue(pr PR, o EnqueueOptions) error
	// ListPool returns open PRs carrying label, with size, author and labels.
	ListPool(label string) ([]PR, error)
	// Queue returns GitHub's merge queue for branch, front first.
	Queue(branch string) ([]QueueEntry, error)
	// ReadyAt is when label was last added to PR n (zero if unknown).
	ReadyAt(n int, label string) (time.Time, error)
	Comment(n int, body string) error
	ClosePR(n int) error
	AddLabels(n int, labels ...string) error
	RemoveLabels(n int, labels ...string) error
}

// GH implements Forge with the gh CLI, which is preinstalled on GitHub
// runners and reads GH_TOKEN / GH_REPO from the environment.
type GH struct {
	Dir  string
	Repo string // optional owner/name; otherwise gh infers it from the git remote
	// Log receives notes about fallbacks, such as the error that made
	// Enqueue use auto-merge. Nil means os.Stderr.
	Log io.Writer
}

func (g *GH) logf(format string, a ...any) {
	w := g.Log
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, format+"\n", a...)
}

func (g *GH) run(stdin string, args ...string) ([]byte, error) {
	cmd := exec.Command("gh", args...)
	cmd.Dir = g.Dir
	cmd.Env = os.Environ()
	if g.Repo != "" {
		cmd.Env = append(cmd.Env, "GH_REPO="+g.Repo)
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh %s: %s", strings.Join(args[:min(3, len(args))], " "), strings.TrimSpace(errb.String()+" "+err.Error()))
	}
	return out.Bytes(), nil
}

const prFields = "number,id,title,body,headRefName,baseRefName,headRefOid,state,isCrossRepository"

type ghPR struct {
	Number      int    `json:"number"`
	ID          string `json:"id"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	HeadRefName string `json:"headRefName"`
	BaseRefName string `json:"baseRefName"`
	HeadRefOid  string `json:"headRefOid"`
	State       string `json:"state"`
	// True when the head branch is in a fork.
	IsCrossRepository bool `json:"isCrossRepository"`
}

func (p ghPR) pr() PR {
	return PR{Number: p.Number, NodeID: p.ID, Title: p.Title, Body: p.Body, HeadRef: p.HeadRefName,
		BaseRef: p.BaseRefName, HeadSHA: p.HeadRefOid, State: p.State, CrossRepo: p.IsCrossRepository}
}

func (g *GH) list(state, limit string) ([]PR, error) {
	out, err := g.run("", "pr", "list", "--state", state, "--limit", limit, "--json", prFields)
	if err != nil {
		return nil, err
	}
	var raw []ghPR
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	prs := make([]PR, len(raw))
	for i, p := range raw {
		prs[i] = p.pr()
	}
	return prs, nil
}

func (g *GH) ListPRs(state string) ([]PR, error) {
	if state == "closed" {
		return g.list("closed", "200") // closed and merged, newest first
	}
	prs, err := g.list("open", "500")
	if err != nil || state != "all" {
		return prs, err
	}
	merged, err := g.list("merged", "200")
	if err != nil {
		return nil, err
	}
	prs = append(prs, merged...)
	sort.Slice(prs, func(i, j int) bool { return prs[i].Number < prs[j].Number })
	return prs, nil
}

func (g *GH) GetPR(n int) (PR, error) {
	out, err := g.run("", "pr", "view", strconv.Itoa(n), "--json", prFields)
	if err != nil {
		return PR{}, err
	}
	var p ghPR
	if err := json.Unmarshal(out, &p); err != nil {
		return PR{}, err
	}
	return p.pr(), nil
}

type ghDetails struct {
	LatestReviews []struct {
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
		State string `json:"state"`
	} `json:"latestReviews"`
	StatusCheckRollup []struct {
		Status     string `json:"status"`     // CheckRun: QUEUED, IN_PROGRESS, COMPLETED
		Conclusion string `json:"conclusion"` // CheckRun: SUCCESS, FAILURE, NEUTRAL, SKIPPED, ...
		State      string `json:"state"`      // StatusContext: SUCCESS, PENDING, FAILURE, ERROR, EXPECTED
	} `json:"statusCheckRollup"`
	MergeCommit *struct {
		Oid string `json:"oid"`
	} `json:"mergeCommit"`
	Commits          []json.RawMessage `json:"commits"`
	HeadRefOid       string            `json:"headRefOid"`
	AutoMergeRequest *struct{}         `json:"autoMergeRequest"`
}

// commitChecks lists the check runs and commit statuses on a commit. The PR's
// statusCheckRollup leaves out runs started by workflow_dispatch (which is how
// a stack branch gets its CI when the built-in token pushed it), so the commit
// is asked directly.
func (g *GH) commitChecks(sha string) ([]string, error) {
	var states []string
	out, err := g.run("", "api", "repos/{owner}/{repo}/commits/"+sha+"/check-runs?per_page=100")
	if err != nil {
		return nil, err
	}
	var runs struct {
		CheckRuns []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal(out, &runs); err != nil {
		return nil, err
	}
	for _, r := range runs.CheckRuns {
		if r.Status != "completed" {
			states = append(states, "PENDING")
		} else {
			states = append(states, r.Conclusion)
		}
	}
	out, err = g.run("", "api", "repos/{owner}/{repo}/commits/"+sha+"/status")
	if err != nil {
		return nil, err
	}
	var st struct {
		Statuses []struct {
			State string `json:"state"`
		} `json:"statuses"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return nil, err
	}
	for _, s := range st.Statuses {
		states = append(states, s.State)
	}
	return states, nil
}

func (g *GH) Details(n int) (Details, error) {
	out, err := g.run("", "pr", "view", strconv.Itoa(n), "--json", "latestReviews,statusCheckRollup,mergeCommit,commits,headRefOid,autoMergeRequest")
	if err != nil {
		return Details{}, err
	}
	var raw ghDetails
	if err := json.Unmarshal(out, &raw); err != nil {
		return Details{}, err
	}
	var d Details
	for _, r := range raw.LatestReviews {
		switch r.State {
		case "APPROVED":
			d.Approvals = append(d.Approvals, r.Author.Login)
		case "CHANGES_REQUESTED":
			d.ChangesRequested = append(d.ChangesRequested, r.Author.Login)
		}
	}
	var states []string
	for _, c := range raw.StatusCheckRollup {
		switch {
		case c.State != "":
			states = append(states, c.State)
		case c.Status != "" && c.Status != "COMPLETED":
			states = append(states, "PENDING")
		default:
			states = append(states, c.Conclusion)
		}
	}
	if raw.HeadRefOid != "" {
		more, err := g.commitChecks(raw.HeadRefOid)
		if err != nil {
			return Details{}, err
		}
		states = append(states, more...)
	}
	d.Checks = RollupChecks(states)
	if raw.MergeCommit != nil {
		d.MergeCommit = raw.MergeCommit.Oid
	}
	d.Commits = len(raw.Commits)
	d.AutoMerge = raw.AutoMergeRequest != nil
	return d, nil
}

// RollupChecks reduces individual check states to one verdict.
func RollupChecks(states []string) string {
	if len(states) == 0 {
		return "none"
	}
	verdict := "success"
	for _, s := range states {
		switch strings.ToUpper(s) {
		case "SUCCESS", "NEUTRAL", "SKIPPED":
		case "PENDING", "EXPECTED", "QUEUED", "IN_PROGRESS", "WAITING", "REQUESTED", "":
			if verdict == "success" {
				verdict = "pending"
			}
		default: // FAILURE, ERROR, CANCELLED, TIMED_OUT, ACTION_REQUIRED, STALE, STARTUP_FAILURE
			return "failure"
		}
	}
	return verdict
}

func (g *GH) FindOpenPRByHead(head string) (*PR, error) {
	out, err := g.run("", "pr", "list", "--state", "open", "--head", head, "--json", prFields)
	if err != nil {
		return nil, err
	}
	var raw []ghPR
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	for _, p := range raw {
		if p.HeadRefName == head {
			pr := p.pr()
			return &pr, nil
		}
	}
	return nil, nil
}

var prURLRe = regexp.MustCompile(`/pull/(\d+)`)

func (g *GH) CreatePR(head, base, title, body string, draft bool) (PR, error) {
	args := []string{"pr", "create", "--head", head, "--base", base, "--title", title, "--body-file", "-"}
	if draft {
		args = append(args, "--draft")
	}
	out, err := g.run(body, args...)
	if err != nil {
		return PR{}, err
	}
	m := prURLRe.FindSubmatch(out)
	if m == nil {
		return PR{}, fmt.Errorf("could not find PR number in gh output: %s", out)
	}
	n, _ := strconv.Atoi(string(m[1]))
	return g.GetPR(n)
}

func (g *GH) EditPR(n int, base, body *string) error {
	args := []string{"pr", "edit", strconv.Itoa(n)}
	stdin := ""
	if base != nil {
		args = append(args, "--base", *base)
	}
	if body != nil {
		args = append(args, "--body-file", "-")
		stdin = *body
		if stdin == "" {
			stdin = " "
		}
	}
	if len(args) == 3 {
		return nil
	}
	_, err := g.run(stdin, args...)
	return err
}

const enqueueMutation = `mutation($id: ID!, $oid: GitObjectID, $jump: Boolean) {
  enqueuePullRequest(input: {pullRequestId: $id, expectedHeadOid: $oid, jump: $jump}) { mergeQueueEntry { position } }
}`

const autoMergeMutation = `mutation($id: ID!) {
  enablePullRequestAutoMerge(input: {pullRequestId: $id}) { pullRequest { number } }
}`

// EnqueueError is a refusal whose cause GitHub's message names and that
// auto-merge would hit too, so retrying with auto-merge only adds noise (and,
// for a stale head, would skip the head check).
type EnqueueError struct {
	PR     int
	Kind   string // closed, draft, stale, missing or forbidden
	Detail string // GitHub's own message
}

func (e *EnqueueError) Error() string {
	switch e.Kind {
	case "closed":
		return fmt.Sprintf("#%d is already merged or closed (GitHub: %s)", e.PR, e.Detail)
	case "draft":
		return fmt.Sprintf("#%d is a draft; mark it ready for review (GitHub: %s)", e.PR, e.Detail)
	case "stale":
		return fmt.Sprintf("#%d changed since it was checked, so it was not queued; stackq will look at it again on its next run (GitHub: %s)", e.PR, e.Detail)
	case "missing":
		return fmt.Sprintf("#%d was not found (GitHub: %s)", e.PR, e.Detail)
	default:
		return fmt.Sprintf("the token may not queue #%d: give the workflow contents: write and pull-requests: write, or use an App or PAT token as STACKQ_TOKEN (GitHub: %s)", e.PR, e.Detail)
	}
}

// IsPermanent reports whether err is an EnqueueError: retrying the same
// request, or enabling auto-merge instead, cannot work.
func IsPermanent(err error) bool {
	var e *EnqueueError
	return errors.As(err, &e)
}

// classifyEnqueue maps GitHub's wording (seen on real runs, 2026-10-26) to an
// EnqueueError, or nil when the message is not one we know to be final. Any
// other message, such as pending checks, falls back to auto-merge.
func classifyEnqueue(pr int, msg string) *EnqueueError {
	m := strings.ToLower(msg)
	kind := ""
	switch {
	case strings.Contains(m, "is closed"), strings.Contains(m, "already merged"):
		kind = "closed"
	case strings.Contains(m, "is in draft"), strings.Contains(m, "is a draft"):
		kind = "draft"
	case strings.Contains(m, "expected head oid does not match"):
		kind = "stale"
	case strings.Contains(m, "could not resolve to a node"):
		kind = "missing"
	case strings.Contains(m, "resource not accessible by integration"):
		kind = "forbidden"
	default:
		return nil
	}
	return &EnqueueError{PR: pr, Kind: kind, Detail: strings.TrimSpace(msg)}
}

// Enqueue tries to add the PR to the merge queue directly. If GitHub refuses
// for a reason that auto-merge would share (the PR is closed, a draft, moved
// on, missing, or the token may not queue it), it returns an EnqueueError.
// For anything else (usually because required checks on the freshly pushed
// head are still running), it enables auto-merge instead, which on a
// merge-queue branch means "join the queue when ready".
func (g *GH) Enqueue(pr PR, o EnqueueOptions) error {
	if pr.NodeID == "" {
		full, err := g.GetPR(pr.Number)
		if err != nil {
			return err
		}
		pr.NodeID = full.NodeID
	}
	args := []string{"api", "graphql", "-f", "query=" + enqueueMutation, "-f", "id=" + pr.NodeID}
	if pr.HeadSHA != "" {
		args = append(args, "-f", "oid="+pr.HeadSHA)
	}
	args = append(args, "-F", "jump="+strconv.FormatBool(o.Jump))
	_, errQ := g.run("", args...)
	if errQ == nil {
		return nil
	}
	if ce := classifyEnqueue(pr.Number, errQ.Error()); ce != nil {
		return ce
	}
	if o.Jump {
		// Auto-merge can't jump; let the caller decide.
		return fmt.Errorf("could not jump #%d to the front of the queue: %v", pr.Number, errQ)
	}
	_, errA := g.run("", "api", "graphql", "-f", "query="+autoMergeMutation, "-f", "id="+pr.NodeID)
	if errA == nil {
		// Auto-merge is not the queue: GitHub adds the PR once its own rules
		// are met, so say why the direct enqueue was not used.
		g.logf("! could not enqueue #%d directly (%v); enabled auto-merge instead, GitHub queues it once its requirements are met",
			pr.Number, strings.TrimSpace(errQ.Error()))
		return nil
	}
	return fmt.Errorf("could not enqueue #%d: %v; auto-merge fallback also failed: %v", pr.Number, errQ, errA)
}

// API makes one REST GET through gh; path may use the {owner}/{repo}
// placeholders. Not part of Forge: only stackq doctor reads settings this way.
func (g *GH) API(path string) ([]byte, error) { return g.run("", "api", path) }

func (g *GH) ClosePR(n int) error {
	_, err := g.run("", "pr", "close", strconv.Itoa(n))
	return err
}

const poolFields = prFields + ",author,labels,additions,deletions,changedFiles,isDraft,createdAt"

// ListPool lists the open PRs that carry label. It filters here instead of
// passing --label: gh turns that flag into a search query, and the search
// index trails label changes by seconds, so a PR labelled a moment ago was
// missing and sat in the pool until some later event ran another pass.
func (g *GH) ListPool(label string) ([]PR, error) {
	out, err := g.run("", "pr", "list", "--state", "open", "--limit", "500", "--json", poolFields)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ghPR
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		Additions    int       `json:"additions"`
		Deletions    int       `json:"deletions"`
		ChangedFiles int       `json:"changedFiles"`
		IsDraft      bool      `json:"isDraft"`
		CreatedAt    time.Time `json:"createdAt"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	prs := make([]PR, len(raw))
	for i, r := range raw {
		p := r.ghPR.pr()
		p.Author, p.Additions, p.Deletions, p.ChangedFiles = r.Author.Login, r.Additions, r.Deletions, r.ChangedFiles
		p.Draft, p.CreatedAt = r.IsDraft, r.CreatedAt
		for _, l := range r.Labels {
			p.Labels = append(p.Labels, l.Name)
		}
		prs[i] = p
	}
	pool := prs[:0]
	for _, p := range prs {
		if slices.Contains(p.Labels, label) {
			pool = append(pool, p)
		}
	}
	return pool, nil
}

const queueQuery = `query($owner: String!, $name: String!, $branch: String!) {
  repository(owner: $owner, name: $name) {
    mergeQueue(branch: $branch) {
      entries(first: 100) { nodes { position state pullRequest { number labels(first: 50) { nodes { name } } } } }
    }
  }
}`

func (g *GH) Queue(branch string) ([]QueueEntry, error) {
	out, err := g.run("", "api", "graphql", "-f", "query="+queueQuery,
		"-F", "owner={owner}", "-F", "name={repo}", "-f", "branch="+branch)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Data struct {
			Repository struct {
				MergeQueue *struct {
					Entries struct {
						Nodes []struct {
							Position    int    `json:"position"`
							State       string `json:"state"`
							PullRequest struct {
								Number int `json:"number"`
								Labels struct {
									Nodes []struct {
										Name string `json:"name"`
									} `json:"nodes"`
								} `json:"labels"`
							} `json:"pullRequest"`
						} `json:"nodes"`
					} `json:"entries"`
				} `json:"mergeQueue"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	mq := raw.Data.Repository.MergeQueue
	if mq == nil {
		return nil, fmt.Errorf("branch %s has no merge queue (turn it on in the branch rules, and check that the token can read it)", branch)
	}
	var entries []QueueEntry
	for _, n := range mq.Entries.Nodes {
		e := QueueEntry{PR: n.PullRequest.Number, Position: n.Position, State: n.State}
		for _, l := range n.PullRequest.Labels.Nodes {
			if l.Name == LabelJumped {
				e.Jumped = true
			}
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Position < entries[j].Position })
	return entries, nil
}

// LabelJumped marks PRs stackq jumped to the front of the queue.
const LabelJumped = "stackq:jumped"

func (g *GH) ReadyAt(n int, label string) (time.Time, error) {
	out, err := g.run("", "api", "--paginate", fmt.Sprintf("repos/{owner}/{repo}/issues/%d/events", n))
	if err != nil {
		return time.Time{}, err
	}
	// --paginate concatenates JSON arrays; decode them one by one.
	dec := json.NewDecoder(bytes.NewReader(out))
	var last time.Time
	for dec.More() {
		var evs []struct {
			Event string `json:"event"`
			Label struct {
				Name string `json:"name"`
			} `json:"label"`
			CreatedAt time.Time `json:"created_at"`
		}
		if err := dec.Decode(&evs); err != nil {
			return time.Time{}, err
		}
		for _, e := range evs {
			if e.Event == "labeled" && e.Label.Name == label && e.CreatedAt.After(last) {
				last = e.CreatedAt
			}
		}
	}
	return last, nil
}

func (g *GH) Comment(n int, body string) error {
	_, err := g.run(body, "pr", "comment", strconv.Itoa(n), "--body-file", "-")
	return err
}

var labelColors = map[string]string{
	"stackq:conflict": "d73a4a", "stackq:blocked": "fbca04", "stackq:integrity": "b60205",
	"stackq:landing": "0e8a16", "stackq:activation": "5319e7",
	"stackq:queue": "c5def5", "stackq:reminded": "fef2c0", "stackq:jumped": "e99695", "stackq:revert": "d93f0b",
}

func (g *GH) AddLabels(n int, labels ...string) error {
	for _, l := range labels {
		color := labelColors[l]
		if color == "" {
			color = "ededed"
		}
		g.run("", "label", "create", l, "--color", color, "--force") // ok if it exists
		if _, err := g.run("", "pr", "edit", strconv.Itoa(n), "--add-label", l); err != nil {
			return err
		}
	}
	return nil
}

func (g *GH) RemoveLabels(n int, labels ...string) error {
	for _, l := range labels {
		g.run("", "pr", "edit", strconv.Itoa(n), "--remove-label", l)
	}
	return nil
}
