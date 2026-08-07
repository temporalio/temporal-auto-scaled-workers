package computeprovider

// FailureClass is the provider-agnostic classification of a compute provider
// failure. Each provider maps its own SDK's error taxonomy onto it so callers can
// distinguish failure kinds without knowing which provider produced the error.
type FailureClass int

const (
	// FailureUnclassified is the zero value, used when a provider has no
	// classification for an error.
	FailureUnclassified FailureClass = iota
	// FailureMisconfigured means the provider rejected the customer's configuration:
	// a resource that does not exist, or credentials that lack permission to it.
	FailureMisconfigured
	// FailureUnavailable means the provider's API was unreachable or returned a
	// server-side error.
	FailureUnavailable
	// FailureThrottled means the request was rate- or concurrency-limited. The
	// configuration is valid; capacity just is not available right now.
	FailureThrottled
	// FailureInternal means the failure is attributable to worker-controller's own
	// configuration or credentials rather than the customer's.
	FailureInternal
)

// ProviderError carries a FailureClass alongside the underlying error. It is
// transparent for message and unwrapping purposes, so wrapping an error in one
// does not change how it reads or how errors.Is/As traverse it.
type ProviderError struct {
	Class FailureClass
	cause error
}

func (e *ProviderError) Error() string { return e.cause.Error() }

func (e *ProviderError) Unwrap() error { return e.cause }

// NewProviderError attaches class to err, returning nil for a nil err so call
// sites can wrap a result unconditionally.
func NewProviderError(class FailureClass, err error) error {
	if err == nil {
		return nil
	}
	return &ProviderError{Class: class, cause: err}
}
