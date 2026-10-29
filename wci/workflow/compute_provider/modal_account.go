package computeprovider

import (
	"context"
	"fmt"
	"os"
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
	UnscopedModalAccountResolver struct {
		// Account is what every namespace resolves to. Empty uses
		// unscopedModalAccount, so the zero value is usable.
		Account string
	}
)

// unscopedModalAccount is what UnscopedModalAccountResolver answers with when it
// was given no account of its own.
//
// Nothing authenticates as it and nothing looks it up anywhere. Credentials are
// cached by (account, workspace), and this is the account half for a deployment
// that has no accounts to tell apart -- a constant, so every namespace in such a
// deployment shares one cache entry per workspace instead of one each.
const unscopedModalAccount = "default"

// namespaceSuffixRE matches "<namespace>.<account id>", the naming convention
// Temporal Cloud namespaces follow. The account is the suffix, so it is the
// second group.
var namespaceSuffixRE = regexp.MustCompile(`^([a-z][a-z0-9-]*[^-])[.]([a-z0-9][a-z0-9-]*[^-])$`)

// envModalAccount names the single account a host serves when its namespaces do not themselves
// carry one. It is read here rather than through fx because a host may embed this package without
// the fx module -- the OSS Temporal server does.
const envModalAccount = "TEMPORAL_MODAL_ACCOUNT"

var (
	modalAccountResolverMu sync.RWMutex
	modalAccountResolver   ModalAccountResolver = UnscopedModalAccountResolver{Account: os.Getenv(envModalAccount)}
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

func (r UnscopedModalAccountResolver) ResolveAccountID(context.Context, string) (string, error) {
	if r.Account != "" {
		return r.Account, nil
	}
	return unscopedModalAccount, nil
}

func (NamespaceSuffixModalAccountResolver) ResolveAccountID(_ context.Context, namespaceName string) (string, error) {
	parts := namespaceSuffixRE.FindStringSubmatch(namespaceName)
	if parts == nil {
		return "", fmt.Errorf("cannot derive an account from namespace %q", namespaceName)
	}
	return parts[2], nil
}

// DefaultModalAccountResolver is the resolver a host gets when it installs none: every namespace
// resolves to the one account TEMPORAL_MODAL_ACCOUNT names, or to a constant cache key when it
// names none. A host whose namespaces carry their own account installs a resolver that reads it.
func DefaultModalAccountResolver() ModalAccountResolver {
	return UnscopedModalAccountResolver{Account: os.Getenv(envModalAccount)}
}
