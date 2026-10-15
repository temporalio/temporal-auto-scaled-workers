package computeprovider

import (
	"errors"

	"go.temporal.io/auto-scaled-workers/wci/compute"
)

// errWCIOwned marks a failure as attributable to worker-controller's own setup
// (base credentials, intermediary role/impersonation chain) rather than the
// customer's config. It has to be marked where it happens: intermediary and
// customer credential steps go through the same helpers and fail
// indistinguishably. Shared across providers and read by their classify* funcs.
var errWCIOwned = errors.New("worker-controller configuration error")

// Aliases to wci/compute.
type (
	FailureClass  = compute.FailureClass
	ProviderError = compute.ProviderError
)

const (
	FailureUnclassified = compute.FailureUnclassified
	FailureRejected     = compute.FailureRejected
	FailureNotFound     = compute.FailureNotFound
	FailureAccessDenied = compute.FailureAccessDenied
	FailureUnavailable  = compute.FailureUnavailable
	FailureThrottled    = compute.FailureThrottled
	FailureInternal     = compute.FailureInternal
	FailureConflict     = compute.FailureConflict
)

var NewProviderError = compute.NewProviderError
