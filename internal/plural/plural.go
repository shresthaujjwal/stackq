// Package plural writes a count with its noun: "1 day", "3 days".
package plural

import "strconv"

// Word returns one when n is 1, and many otherwise. many defaults to
// one + "s"; pass it for nouns like "branch" -> "branches".
func Word(n int, one string, many ...string) string {
	switch {
	case n == 1:
		return one
	case len(many) > 0:
		return many[0]
	}
	return one + "s"
}

// N returns n followed by its noun: "1 day", "3 days".
func N(n int, one string, many ...string) string {
	return strconv.Itoa(n) + " " + Word(n, one, many...)
}
