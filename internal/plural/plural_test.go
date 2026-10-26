package plural

import "testing"

func TestN(t *testing.T) {
	for _, c := range []struct {
		n    int
		one  string
		many []string
		want string
	}{
		{1, "day", nil, "1 day"},
		{0, "day", nil, "0 days"},
		{7, "day", nil, "7 days"},
		{1, "branch", []string{"branches"}, "1 branch"},
		{2, "branch", []string{"branches"}, "2 branches"},
	} {
		if got := N(c.n, c.one, c.many...); got != c.want {
			t.Errorf("N(%d, %q, %v) = %q, want %q", c.n, c.one, c.many, got, c.want)
		}
	}
}
