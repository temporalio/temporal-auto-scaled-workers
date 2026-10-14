package computeprovider

import (
	"context"
	"fmt"
	"regexp"
	"sync"
)

type (
	// ModalAccountResolver maps a namespace to the account that owns it. The Modal
	// provider resolves the account this way rather than reading it out of the
	// worker deployment version's config, so a config can never name a grant
	// belonging to somebody else.
	ModalAccountResolver interface {
		ResolveAccountID(ctx context.Context, namespaceName string) (string, error)
	}

	// NamespaceSuffixModalAccountResolver derives the account from the namespace's
	// own name, which the server supplies. It is the default; a deployment whose
	// namespaces are named differently installs its own resolver through
	// SetModalAccountResolver.
	NamespaceSuffixModalAccountResolver struct{}
)

// namespaceSuffixRE matches "<namespace>.<account id>", the internal namespace
// naming convention. The account is the suffix, so it is the second group.
var namespaceSuffixRE = regexp.MustCompile(`^([a-z][a-z0-9-]*[^-])[.]([a-z0-9][a-z0-9-]*[^-])$`)

var (
	modalAccountResolverMu sync.RWMutex
	modalAccountResolver   ModalAccountResolver = NamespaceSuffixModalAccountResolver{}
)

// SetModalAccountResolver installs the process-wide account resolver used by the
// Modal compute provider. Called once at startup; a nil resolver is ignored so
// the default survives.
func SetModalAccountResolver(r ModalAccountResolver) {
	modalAccountResolverMu.Lock()
	defer modalAccountResolverMu.Unlock()
	if r != nil {
		modalAccountResolver = r
	}
}

func getModalAccountResolver() ModalAccountResolver {
	modalAccountResolverMu.RLock()
	defer modalAccountResolverMu.RUnlock()
	return modalAccountResolver
}

func (NamespaceSuffixModalAccountResolver) ResolveAccountID(_ context.Context, namespaceName string) (string, error) {
	parts := namespaceSuffixRE.FindStringSubmatch(namespaceName)
	if parts == nil {
		return "", fmt.Errorf("cannot derive an account from namespace %q", namespaceName)
	}
	return parts[2], nil
}
