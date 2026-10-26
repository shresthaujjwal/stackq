package land_test

import (
	"strings"
	"testing"

	"stackq/internal/forge"
	"stackq/internal/land"
	"stackq/internal/stack"
	"stackq/internal/testrepo"
)

// When GitHub will not queue the lander (auto-merge is waiting for what the PR
// itself needs), the people who approved the parts are asked to review it.
func TestLanderWaitingOnGitHubAsksThePartApproversToReviewIt(t *testing.T) {
	approved := func(fx *fixture, i int, who ...string) {
		fx.f.SetDetails(prBy(t, fx, i).Number, func(d *forge.Details) { d.Approvals = who })
	}
	waiting := func(allowUnreviewed bool) (*fixture, string) {
		fx := setupWith(t, testrepo.FlagApp(t, "if"), shopFlags, stack.Atomic)
		approved(fx, 0, "alice")
		approved(fx, 1, "bob", "alice")
		approved(fx, 2, "carol") // the lander, approved at its own diff
		run := &land.Runner{Repo: fx.dev, Forge: fx.f, Remote: "origin", AllowUnreviewed: allowUnreviewed}
		if err := run.AutoLand(); err != nil {
			t.Fatal(err)
		}
		lander := prBy(t, fx, 2)
		fx.f.MQ = nil
		fx.f.SetDetails(lander.Number, func(d *forge.Details) { d.AutoMerge = true })
		for i := 0; i < 2; i++ { // a second tick must change nothing
			if err := run.AutoLand(); err != nil {
				t.Fatal(err)
			}
		}
		return fx, prBy(t, fx, 2).Body
	}

	fx, body := waiting(false)
	lander := prBy(t, fx, 2).Number
	if got := strings.Join(fx.f.ReviewRequests[lander][:2], ","); got != "alice,bob" {
		t.Fatalf("want a review requested from alice and bob (not carol, who approved the lander), got %v", fx.f.ReviewRequests[lander])
	}
	for _, who := range fx.f.ReviewRequests[lander] {
		if who == "carol" {
			t.Fatalf("carol already approved the lander: %v", fx.f.ReviewRequests[lander])
		}
	}
	if !strings.Contains(body, "Waiting for GitHub's requirements") || !strings.Contains(body, "Asked for a review of this PR from alice, bob") {
		t.Fatalf("the status should say who was asked:\n%s", body)
	}
	if strings.Count(body, "Asked for a review") != 1 || len(fx.f.Comments[lander]) > 1 {
		t.Fatalf("repeated ticks must not pile up notes or comments (comments=%d):\n%s", len(fx.f.Comments[lander]), body)
	}

	fx2, body2 := waiting(true)
	if got := fx2.f.ReviewRequests[prBy(t, fx2, 2).Number]; len(got) != 0 || strings.Contains(body2, "Asked for a review") {
		t.Fatalf("with allowUnreviewed nobody is asked: %v", got)
	}
}
