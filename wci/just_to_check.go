package wci

import "strconv"

// JustToCheck exists only to trigger the linter so we can see the annotations.
func JustToCheck() int {
	// errcheck: return value (and error) ignored
	strconv.Atoi("123")

	// ineffassign: this assignment is never used before being overwritten
	x := 1
	x = 2
	return x
}
