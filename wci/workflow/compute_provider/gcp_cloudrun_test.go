package computeprovider

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"

	"go.temporal.io/auto-scaled-workers/wci/client"
)

type impersonateCall struct {
	cfg     impersonate.CredentialsConfig
	optsLen int
}

// stubImpersonate swaps the package impersonation seam with a recorder that
// returns a dummy token source, so tests can assert how the chain is built
// (base credential vs. Delegates) without real GCP auth. Restores on cleanup.
func stubImpersonate(t *testing.T) *[]impersonateCall {
	t.Helper()
	var calls []impersonateCall
	orig := impersonateTokenSourceFn
	impersonateTokenSourceFn = func(_ context.Context, cfg impersonate.CredentialsConfig, opts ...option.ClientOption) (oauth2.TokenSource, error) {
		calls = append(calls, impersonateCall{cfg: cfg, optsLen: len(opts)})
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake"}), nil
	}
	t.Cleanup(func() { impersonateTokenSourceFn = orig })
	return &calls
}

// TestGCPCloudRun_GlobalSAConsumedAsBaseNotDelegate asserts the delegates[0]
// (pool serverless-global-sa) is established as the chain BASE via direct
// impersonation — no base opts, empty Delegates — and only delegates[1:] reach
// the target impersonation as Delegates, with the base token source threaded in.
func TestGCPCloudRun_GlobalSAConsumedAsBaseNotDelegate(t *testing.T) {
	setChainProviderForTest(t, &captureChainProvider{delegates: []string{"global@pool.iam.gserviceaccount.com", "acct@pool.iam.gserviceaccount.com"}})
	calls := stubImpersonate(t)

	p := &gcpCloudRunComputeProvider{}
	// Return values discarded: the downstream Cloud Run client construction is
	// irrelevant here — we assert only on the captured impersonation calls.
	_, _, _ = p.buildClientAndParams(t.Context(), RequestContext{NamespaceName: "myns.acct"}, ComputeProviderConfig{
		configGCPCloudRunProject:        "p",
		configGCPCloudRunRegion:         "r",
		configGCPCloudRunWorkerPool:     "wp",
		configGCPCloudRunServiceAccount: "cust@example.com",
	})

	require.Len(t, *calls, 2, "expected base + target impersonation calls")
	base, target := (*calls)[0], (*calls)[1]

	assert.Equal(t, "global@pool.iam.gserviceaccount.com", base.cfg.TargetPrincipal, "base hop should target the global SA")
	assert.Empty(t, base.cfg.Delegates, "base hop must be a direct impersonation (no Delegates)")
	assert.Zero(t, base.optsLen, "base hop must use the ambient ADC (no base token source)")

	assert.Equal(t, "cust@example.com", target.cfg.TargetPrincipal, "target hop should target the customer SA")
	assert.Equal(t, []string{"acct@pool.iam.gserviceaccount.com"}, target.cfg.Delegates, "target hop Delegates should be delegates[1:]")
	assert.Equal(t, 1, target.optsLen, "target hop must receive the base token source")
}

// TestGCPCloudRun_SingleElementChainBaseThenDirectTarget asserts the boundary
// where the chain has exactly one entry (the global SA): it is consumed as the
// base, leaving delegates[1:] as an empty slice, so the target hop impersonates
// the customer directly *from the global SA base* — base opts present, empty
// Delegates. Guards the chainDelegates[1:] slicing on a length-1 input.
func TestGCPCloudRun_SingleElementChainBaseThenDirectTarget(t *testing.T) {
	setChainProviderForTest(t, &captureChainProvider{delegates: []string{"global@pool.iam.gserviceaccount.com"}})
	calls := stubImpersonate(t)

	p := &gcpCloudRunComputeProvider{}
	_, _, _ = p.buildClientAndParams(t.Context(), RequestContext{NamespaceName: "myns.acct"}, ComputeProviderConfig{
		configGCPCloudRunProject:        "p",
		configGCPCloudRunRegion:         "r",
		configGCPCloudRunWorkerPool:     "wp",
		configGCPCloudRunServiceAccount: "cust@example.com",
	})

	require.Len(t, *calls, 2, "expected base + target impersonation calls")
	base, target := (*calls)[0], (*calls)[1]

	assert.Equal(t, "global@pool.iam.gserviceaccount.com", base.cfg.TargetPrincipal, "base hop should directly impersonate the global SA")
	assert.Empty(t, base.cfg.Delegates, "base hop must be a direct impersonation (no Delegates)")
	assert.Zero(t, base.optsLen, "base hop must use the ambient ADC (no base token source)")

	assert.Equal(t, "cust@example.com", target.cfg.TargetPrincipal, "target hop should target the customer SA")
	assert.Empty(t, target.cfg.Delegates, "target hop must have empty Delegates when the chain has one entry")
	assert.Equal(t, 1, target.optsLen, "target hop must still receive the global SA base token source")
}

// TestGCPCloudRun_EmptyChainDirectlyImpersonatesTarget asserts that when the
// chain provider returns no delegates (e.g. namespace didn't parse), the target
// is impersonated directly from the ambient ADC — a single call, no base opts.
func TestGCPCloudRun_EmptyChainDirectlyImpersonatesTarget(t *testing.T) {
	setChainProviderForTest(t, &captureChainProvider{delegates: nil})
	calls := stubImpersonate(t)

	p := &gcpCloudRunComputeProvider{}
	_, _, _ = p.buildClientAndParams(t.Context(), RequestContext{NamespaceName: "no-dot-here"}, ComputeProviderConfig{
		configGCPCloudRunProject:        "p",
		configGCPCloudRunRegion:         "r",
		configGCPCloudRunWorkerPool:     "wp",
		configGCPCloudRunServiceAccount: "cust@example.com",
	})

	require.Len(t, *calls, 1, "expected a single direct impersonation call")
	only := (*calls)[0]
	assert.Equal(t, "cust@example.com", only.cfg.TargetPrincipal, "should impersonate the customer SA directly")
	assert.Empty(t, only.cfg.Delegates, "expected no Delegates on empty chain")
	assert.Zero(t, only.optsLen, "expected direct impersonation from ambient ADC (no base opts)")
}

type captureChainProvider struct {
	capture   *ResolveChainInput
	delegates []string
	err       error
}

func (c *captureChainProvider) ResolveChain(_ context.Context, input ResolveChainInput) ([]string, error) {
	if c.capture != nil {
		*c.capture = input
	}
	return c.delegates, c.err
}

// setChainProviderForTest installs cp as the process-wide chain provider for the
// duration of the test, restoring the no-op default on cleanup.
func setChainProviderForTest(t *testing.T, cp GCPImpersonationChainProvider) {
	t.Helper()
	SetGCPImpersonationChainProvider(cp)
	t.Cleanup(func() { SetGCPImpersonationChainProvider(NoopGCPImpersonationChainProvider{}) })
}

func TestGCPCloudRun_ChainProviderReceivesNamespaceAndFlattenedCandidates(t *testing.T) {
	var captured ResolveChainInput
	setChainProviderForTest(t, &captureChainProvider{capture: &captured, delegates: []string{"d1"}})
	p := &gcpCloudRunComputeProvider{
		intermediaryServiceAccounts: [][]client.GCPIAMServiceAccountRequest{
			{{ServiceAccountEmail: "sa-a"}, {ServiceAccountEmail: "sa-b"}},
			{{ServiceAccountEmail: "sa-c"}},
		},
	}
	// Discard the final return — we only care the chain provider was invoked with
	// the expected input. The downstream Cloud Run client construction may or may
	// not succeed depending on the test env's GCP auth state.
	_, _, _ = p.buildClientAndParams(t.Context(), RequestContext{NamespaceName: "my-ns"}, ComputeProviderConfig{
		configGCPCloudRunProject:        "p",
		configGCPCloudRunRegion:         "r",
		configGCPCloudRunWorkerPool:     "wp",
		configGCPCloudRunServiceAccount: "cust@example.com",
	})
	assert.Equal(t, "my-ns", captured.Namespace, "namespace not threaded")
	assert.Equal(t, [][]string{{"sa-a", "sa-b"}, {"sa-c"}}, captured.GlobalSACandidates, "candidates mismatch")
}

func TestGCPCloudRun_ChainProviderErrorWrapped(t *testing.T) {
	setChainProviderForTest(t, &captureChainProvider{err: errors.New("boom")})
	p := &gcpCloudRunComputeProvider{}
	_, _, err := p.buildClientAndParams(t.Context(), RequestContext{NamespaceName: "my-ns"}, ComputeProviderConfig{
		configGCPCloudRunProject:        "p",
		configGCPCloudRunRegion:         "r",
		configGCPCloudRunWorkerPool:     "wp",
		configGCPCloudRunServiceAccount: "cust@example.com",
	})
	require.Error(t, err, "expected wrapped chain-provider error")
	assert.ErrorContains(t, err, "boom")
	assert.ErrorContains(t, err, "impersonation chain")
}

func TestGCPCloudRun_ChainProviderNotCalledWithoutCustomerSA(t *testing.T) {
	called := false
	setChainProviderForTest(t, chainProviderFunc(func(context.Context, ResolveChainInput) ([]string, error) {
		called = true
		return nil, nil
	}))
	p := &gcpCloudRunComputeProvider{}
	_, _, _ = p.buildClientAndParams(t.Context(), RequestContext{NamespaceName: "my-ns"}, ComputeProviderConfig{
		configGCPCloudRunProject:    "p",
		configGCPCloudRunRegion:     "r",
		configGCPCloudRunWorkerPool: "wp",
		// no service_account → no impersonation chain needed
	})
	assert.False(t, called, "chain provider should not be called when customer service account is absent")
}

type chainProviderFunc func(context.Context, ResolveChainInput) ([]string, error)

func (f chainProviderFunc) ResolveChain(ctx context.Context, input ResolveChainInput) ([]string, error) {
	return f(ctx, input)
}

func TestNoopGCPImpersonationChainProvider_Empty(t *testing.T) {
	delegates, err := (NoopGCPImpersonationChainProvider{}).ResolveChain(t.Context(), ResolveChainInput{})
	require.NoError(t, err)
	assert.Empty(t, delegates, "expected empty Delegates")
}

func TestNoopGCPImpersonationChainProvider_SingleHopSingleCandidate(t *testing.T) {
	delegates, err := (NoopGCPImpersonationChainProvider{}).ResolveChain(t.Context(), ResolveChainInput{
		GlobalSACandidates: [][]string{{"sa-a"}},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"sa-a"}, delegates)
}

func TestNoopGCPImpersonationChainProvider_MultiHopOrderPreserved(t *testing.T) {
	delegates, err := (NoopGCPImpersonationChainProvider{}).ResolveChain(t.Context(), ResolveChainInput{
		GlobalSACandidates: [][]string{{"hop-1"}, {"hop-2"}, {"hop-3"}},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"hop-1", "hop-2", "hop-3"}, delegates, "expected ordered hops")
}

func TestNoopGCPImpersonationChainProvider_EmptyStepSkipped(t *testing.T) {
	delegates, err := (NoopGCPImpersonationChainProvider{}).ResolveChain(t.Context(), ResolveChainInput{
		GlobalSACandidates: [][]string{{"a"}, {}, {"b"}},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, delegates, "expected empty step skipped")
}

func TestNoopGCPImpersonationChainProvider_EmptyEntryErrors(t *testing.T) {
	_, err := (NoopGCPImpersonationChainProvider{}).ResolveChain(t.Context(), ResolveChainInput{
		GlobalSACandidates: [][]string{{""}},
	})
	require.Error(t, err, "expected error for empty entry")
	assert.ErrorContains(t, err, "empty")
}

func TestNoopGCPImpersonationChainProvider_MultiCandidatePickFromSet(t *testing.T) {
	candidates := []string{"a", "b", "c"}
	for i := 0; i < 10; i++ {
		delegates, err := (NoopGCPImpersonationChainProvider{}).ResolveChain(t.Context(), ResolveChainInput{
			GlobalSACandidates: [][]string{candidates},
		})
		require.NoErrorf(t, err, "iter %d", i)
		require.Lenf(t, delegates, 1, "iter %d: expected 1 delegate", i)
		assert.Containsf(t, candidates, delegates[0], "iter %d: pick not in candidate set", i)
	}
}
