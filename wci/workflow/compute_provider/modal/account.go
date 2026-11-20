package modal

import (
	"fmt"
	"regexp"
	"sync/atomic"
)

// namespaceSuffixRE matches "<namespace>.<account id>".
var namespaceSuffixRE = regexp.MustCompile(`^([a-z][a-z0-9-]*[^-])[.]([a-z0-9][a-z0-9-]*[^-])$`)

var testAccount atomic.Pointer[string]

// accountFor never reads config, so a config cannot name another account's grant.
func accountFor(namespaceName string) (string, error) {
	if account := testAccount.Load(); account != nil {
		return *account, nil
	}
	parts := namespaceSuffixRE.FindStringSubmatch(namespaceName)
	if parts == nil {
		return "", fmt.Errorf("cannot derive an account from namespace %q", namespaceName)
	}
	return parts[2], nil
}
