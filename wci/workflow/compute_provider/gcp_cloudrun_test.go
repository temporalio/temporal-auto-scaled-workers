package computeprovider

import (
	"context"
	"errors"
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

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

// TestGCPCloudRun_ImpersonationMatrix exercises how buildClientAndParams
// constructs the impersonation calls across the first-delegate-as-base flag and
// the size of the resolved delegate chain (nil / zero / one / more-than-one).
//
// With the flag ON, delegates[0] is the chain base: it is directly impersonated
// (its own call, ambient ADC, no Delegates) and delegates[1:] become the target
// hop's token-creator Delegates, with the base token source threaded in as one
// client option. With the flag OFF, the whole chain is passed as Delegates from
// the ambient ADC in a single call. nil and zero-length chains are equivalent
// (both len 0), so no base hop runs regardless of the flag.
func TestGCPCloudRun_ImpersonationMatrix(t *testing.T) {
	const target = "cust@example.com"
	const g = "global@pool.iam.gserviceaccount.com"
	const a = "acct@pool.iam.gserviceaccount.com"

	cases := []struct {
		name        string
		asBase      bool
		delegates   []string
		wantCalls   int
		wantBase    string   // base hop target principal; "" when there is no base hop
		wantDelegs  []string // Delegates on the final (target) impersonation call
		wantBaseOpt bool     // whether the base token source is threaded into the target call
	}{
		// Flag ON: delegates[0] consumed as the chain base, delegates[1:] as delegates.
		{"base/nil", true, nil, 1, "", nil, false},
		{"base/zero", true, []string{}, 1, "", nil, false},
		{"base/one", true, []string{g}, 2, g, nil, true},
		{"base/many", true, []string{g, a}, 2, g, []string{a}, true},
		// Flag OFF: whole chain passed as delegates from ambient ADC, no base hop.
		{"nobase/nil", false, nil, 1, "", nil, false},
		{"nobase/zero", false, []string{}, 1, "", nil, false},
		{"nobase/one", false, []string{g}, 1, "", []string{g}, false},
		{"nobase/many", false, []string{g, a}, 1, "", []string{g, a}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setChainProviderForTest(t, &captureChainProvider{delegates: tc.delegates})
			calls := stubImpersonate(t)

			p := &gcpCloudRunComputeProvider{firstDelegateAsBase: tc.asBase}
			// Return values discarded: we assert only on the captured impersonation calls.
			_, _, _ = p.buildClientAndParams(t.Context(), RequestContext{NamespaceName: "myns.acct"}, ComputeProviderConfig{
				configGCPCloudRunProject:        "p",
				configGCPCloudRunRegion:         "r",
				configGCPCloudRunWorkerPool:     "wp",
				configGCPCloudRunServiceAccount: target,
			})

			require.Len(t, *calls, tc.wantCalls, "impersonation call count")

			// The target (customer) impersonation is always the last call.
			targetCall := (*calls)[len(*calls)-1]
			assert.Equal(t, target, targetCall.cfg.TargetPrincipal, "final hop must target the customer SA")
			if len(tc.wantDelegs) == 0 {
				assert.Empty(t, targetCall.cfg.Delegates, "final hop Delegates should be empty")
			} else {
				assert.Equal(t, tc.wantDelegs, targetCall.cfg.Delegates, "final hop Delegates")
			}
			wantOpts := 0
			if tc.wantBaseOpt {
				wantOpts = 1
			}
			assert.Equal(t, wantOpts, targetCall.optsLen, "final hop opts (1 => base token source threaded in)")

			// A base hop only exists when the first-delegate-as-base flag consumed delegates[0].
			if tc.wantBase != "" {
				require.Len(t, *calls, 2, "expected a base hop before the target hop")
				base := (*calls)[0]
				assert.Equal(t, tc.wantBase, base.cfg.TargetPrincipal, "base hop must directly impersonate delegates[0]")
				assert.Empty(t, base.cfg.Delegates, "base hop must be a direct impersonation (no Delegates)")
				assert.Zero(t, base.optsLen, "base hop must use ambient ADC (no base token source)")
			}
		})
	}
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

// TestUpdateWorkerPoolMaskResolvesAgainstDescriptor is the regression guard for the
// silent no-op scaling bug: the update mask must use the proto field name (snake_case)
// so it resolves server-side over gRPC, which transmits FieldMask paths verbatim.
//
// FieldMask.IsValid performs the same lookup the Cloud Run server does — it walks each
// path segment via the message descriptor's proto field names. A camelCase path
// ("manualInstanceCount") does not resolve, so the field is dropped and instances never
// scale.
func TestUpdateWorkerPoolMaskResolvesAgainstDescriptor(t *testing.T) {
	req := buildUpdateWorkerPoolRequest("projects/p/locations/r/workerPools/wp", 3)

	// The production mask resolves against the real WorkerPool descriptor.
	require.True(t, req.GetUpdateMask().IsValid(req.GetWorkerPool()),
		"production update mask %v must resolve against the WorkerPool proto", req.GetUpdateMask().GetPaths())
	assert.Equal(t, []string{"scaling.manual_instance_count"}, req.GetUpdateMask().GetPaths(),
		"mask path must be the snake_case proto field name")

	// Demonstrate the bug: the JSON/camelCase form does NOT resolve over the proto
	// transport, which is why the previous mask produced a granted-but-no-op update.
	buggy := &fieldmaskpb.FieldMask{Paths: []string{"scaling.manualInstanceCount"}}
	assert.False(t, buggy.IsValid(&runpb.WorkerPool{}),
		"camelCase mask path must NOT resolve — this is the no-op scaling bug")
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
