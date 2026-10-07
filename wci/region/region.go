package region

import "regexp"

// idPattern allows lowercase letters and digits separated by single hyphens.
var idPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidRegionID returns true if id is a well-formed region ID.
func ValidRegionID(id string) bool {
	return idPattern.MatchString(id)
}
