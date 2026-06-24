package computeprovider

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go.temporal.io/auto-scaled-workers/wci/client"
)

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
	_, _, _ = p.buildClientAndParams(context.Background(), RequestContext{NamespaceName: "my-ns"}, ComputeProviderConfig{
		configGCPCloudRunProject:        "p",
		configGCPCloudRunRegion:         "r",
		configGCPCloudRunWorkerPool:     "wp",
		configGCPCloudRunServiceAccount: "cust@example.com",
	})
	if captured.Namespace != "my-ns" {
		t.Errorf("namespace not threaded: got %q, want %q", captured.Namespace, "my-ns")
	}
	want := [][]string{{"sa-a", "sa-b"}, {"sa-c"}}
	if !reflect.DeepEqual(captured.GlobalSACandidates, want) {
		t.Errorf("candidates mismatch: got %v, want %v", captured.GlobalSACandidates, want)
	}
}

func TestGCPCloudRun_ChainProviderErrorWrapped(t *testing.T) {
	setChainProviderForTest(t, &captureChainProvider{err: errors.New("boom")})
	p := &gcpCloudRunComputeProvider{}
	_, _, err := p.buildClientAndParams(context.Background(), RequestContext{NamespaceName: "my-ns"}, ComputeProviderConfig{
		configGCPCloudRunProject:        "p",
		configGCPCloudRunRegion:         "r",
		configGCPCloudRunWorkerPool:     "wp",
		configGCPCloudRunServiceAccount: "cust@example.com",
	})
	if err == nil {
		t.Fatal("expected wrapped chain-provider error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected wrapped error containing 'boom'; got: %v", err)
	}
	if !strings.Contains(err.Error(), "impersonation chain") {
		t.Errorf("expected 'impersonation chain' in error; got: %v", err)
	}
}

func TestGCPCloudRun_ChainProviderNotCalledWithoutCustomerSA(t *testing.T) {
	called := false
	setChainProviderForTest(t, chainProviderFunc(func(context.Context, ResolveChainInput) ([]string, error) {
		called = true
		return nil, nil
	}))
	p := &gcpCloudRunComputeProvider{}
	_, _, _ = p.buildClientAndParams(context.Background(), RequestContext{NamespaceName: "my-ns"}, ComputeProviderConfig{
		configGCPCloudRunProject:    "p",
		configGCPCloudRunRegion:     "r",
		configGCPCloudRunWorkerPool: "wp",
		// no service_account → no impersonation chain needed
	})
	if called {
		t.Error("chain provider should not be called when customer service account is absent")
	}
}

type chainProviderFunc func(context.Context, ResolveChainInput) ([]string, error)

func (f chainProviderFunc) ResolveChain(ctx context.Context, input ResolveChainInput) ([]string, error) {
	return f(ctx, input)
}

func TestNoopGCPImpersonationChainProvider_Empty(t *testing.T) {
	delegates, err := (NoopGCPImpersonationChainProvider{}).ResolveChain(context.Background(), ResolveChainInput{})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(delegates) != 0 {
		t.Errorf("expected empty Delegates, got: %v", delegates)
	}
}

func TestNoopGCPImpersonationChainProvider_SingleHopSingleCandidate(t *testing.T) {
	delegates, err := (NoopGCPImpersonationChainProvider{}).ResolveChain(context.Background(), ResolveChainInput{
		GlobalSACandidates: [][]string{{"sa-a"}},
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if !reflect.DeepEqual(delegates, []string{"sa-a"}) {
		t.Errorf("expected [sa-a], got: %v", delegates)
	}
}

func TestNoopGCPImpersonationChainProvider_MultiHopOrderPreserved(t *testing.T) {
	delegates, err := (NoopGCPImpersonationChainProvider{}).ResolveChain(context.Background(), ResolveChainInput{
		GlobalSACandidates: [][]string{{"hop-1"}, {"hop-2"}, {"hop-3"}},
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if !reflect.DeepEqual(delegates, []string{"hop-1", "hop-2", "hop-3"}) {
		t.Errorf("expected ordered hops, got: %v", delegates)
	}
}

func TestNoopGCPImpersonationChainProvider_EmptyStepSkipped(t *testing.T) {
	delegates, err := (NoopGCPImpersonationChainProvider{}).ResolveChain(context.Background(), ResolveChainInput{
		GlobalSACandidates: [][]string{{"a"}, {}, {"b"}},
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if !reflect.DeepEqual(delegates, []string{"a", "b"}) {
		t.Errorf("expected empty step skipped, got: %v", delegates)
	}
}

func TestNoopGCPImpersonationChainProvider_EmptyEntryErrors(t *testing.T) {
	_, err := (NoopGCPImpersonationChainProvider{}).ResolveChain(context.Background(), ResolveChainInput{
		GlobalSACandidates: [][]string{{""}},
	})
	if err == nil {
		t.Fatal("expected error for empty entry")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should mention 'empty'; got: %v", err)
	}
}

func TestNoopGCPImpersonationChainProvider_MultiCandidatePickFromSet(t *testing.T) {
	candidates := []string{"a", "b", "c"}
	valid := map[string]bool{"a": true, "b": true, "c": true}
	for i := 0; i < 10; i++ {
		delegates, err := (NoopGCPImpersonationChainProvider{}).ResolveChain(context.Background(), ResolveChainInput{
			GlobalSACandidates: [][]string{candidates},
		})
		if err != nil {
			t.Fatalf("iter %d: unexpected error: %v", i, err)
		}
		if len(delegates) != 1 {
			t.Fatalf("iter %d: expected 1 delegate, got: %v", i, delegates)
		}
		if !valid[delegates[0]] {
			t.Errorf("iter %d: pick %q not in candidate set", i, delegates[0])
		}
	}
}
