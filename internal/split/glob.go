package split

import (
	"regexp"
	"strings"
)

// compileGlob turns a gitignore-style pattern into a regexp over
// repo-relative paths. "*" stays within a path segment, "**" crosses them,
// a pattern without "/" matches at any depth, and a pattern that names a
// directory also matches everything under it.
func compileGlob(p string) (*regexp.Regexp, error) {
	p = strings.TrimSpace(p)
	dirOnly := strings.HasSuffix(p, "/")
	p = strings.TrimSuffix(p, "/")
	rooted := strings.HasPrefix(p, "/")
	p = strings.TrimPrefix(p, "/")

	var b strings.Builder
	b.WriteString("^")
	if !rooted && !strings.Contains(p, "/") {
		b.WriteString("(?:.*/)?")
	}
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				if i+2 < len(p) && p[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 2
				} else {
					b.WriteString(".*")
					i++
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	if dirOnly {
		b.WriteString("/.*")
	} else {
		b.WriteString("(?:/.*)?")
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
