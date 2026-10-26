package land

import (
	"fmt"
	"sort"
	"strings"

	"stackq/internal/forge"
)

// askForLanderReview is for repositories that require approval, or approval
// by code owners, on the PR that merges. The lander is a PR of its own:
// approvals given on the parts below it are on other PRs, and whether GitHub
// counts any of them for the lander depends on the rules (not confirmed when
// the retarget was tested). It runs only when GitHub would not queue the
// lander (auto-merge is waiting for its requirements), asks the people who
// approved the parts, and who have not approved the lander, to review it, and
// returns a sentence for the status section. It changes no gate: landing is
// decided by the parts, and GitHub decides what the PR itself needs. Nothing
// is asked when approvals are off (allowUnreviewed). Asking someone who was
// already asked changes nothing on GitHub, so it is safe on every tick.
func (c *Runner) askForLanderReview(lander forge.PR, parts []forge.PR) string {
	if c.AllowUnreviewed {
		return ""
	}
	d, err := c.Forge.Details(lander.Number)
	if err != nil {
		return ""
	}
	have := map[string]bool{}
	for _, a := range d.Approvals {
		have[a] = true
	}
	seen := map[string]bool{}
	var ask []string
	for _, p := range parts {
		if p.Number == lander.Number {
			continue
		}
		pd, err := c.Forge.Details(p.Number)
		if err != nil {
			continue
		}
		for _, a := range pd.Approvals {
			if a != "" && !have[a] && !seen[a] {
				seen[a] = true
				ask = append(ask, a)
			}
		}
	}
	sort.Strings(ask)
	if len(ask) == 0 {
		return ""
	}
	refused := c.Forge.RequestReviews(lander.Number, ask)
	var asked []string
	for _, a := range ask {
		if err, bad := refused[a]; bad {
			c.logf("- could not ask %s to review #%d: %v", a, lander.Number, err)
			continue
		}
		asked = append(asked, a)
	}
	if len(asked) == 0 {
		return ""
	}
	c.logf("? #%d is waiting on GitHub's requirements; asked %s to review it", lander.Number, strings.Join(asked, ", "))
	return fmt.Sprintf("\n\n👀 Approvals on the parts are on those PRs, not here. Asked for a review of this PR from %s, who approved the parts. "+
		"If a code owner of any file in the stack has not reviewed, GitHub needs them to.", strings.Join(asked, ", "))
}
