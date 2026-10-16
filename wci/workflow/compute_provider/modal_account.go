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
	// own name, for a deployment whose namespaces are named "<namespace>.<account>".
	NamespaceSuffixModalAccountResolver struct{}

	// UnscopedModalAccountResolver resolves every namespace to the same account.
	// It is the default, and it is what a deployment serving a single Modal
	// workspace wants: there is one set of credentials, so there is nothing to
	// scope them by. A deployment serving many accounts installs a resolver that
	// tells them apart.
	UnscopedModalAccountResolver struct{}
)

// unscopedModalAccount is the account every namespace resolves to by default.
// It is not an identity, just a stable cache key.
const unscopedModalAccount = "default"

// namespaceSuffixRE matches "<namespace>.<account id>", the internal namespace
// naming convention. The account is the suffix, so it is the second group.
var namespaceSuffixRE = regexp.MustCompile(`^([a-z][a-z0-9-]*[^-])[.]([a-z0-9][a-z0-9-]*[^-])$`)

var (
	modalAccountResolverMu sync.RWMutex
	modalAccountResolver   ModalAccountResolver = UnscopedModalAccountResolver{}
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

func (UnscopedModalAccountResolver) ResolveAccountID(context.Context, string) (string, error) {
	return unscopedModalAccount, nil
}

func (NamespaceSuffixModalAccountResolver) ResolveAccountID(_ context.Context, namespaceName string) (string, error) {
	parts := namespaceSuffixRE.FindStringSubmatch(namespaceName)
	if parts == nil {
		return "", fmt.Errorf("cannot derive an account from namespace %q", namespaceName)
	}
	return parts[2], nil
}
