package computeprovider

import (
	"context"
	"errors"
	"testing"

	modal "github.com/modal-labs/modal-client/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeModalFunction struct {
	spawnFn func(ctx context.Context, args []any, kwargs map[string]any) (*modal.FunctionCall, error)
}

func (f *fakeModalFunction) Spawn(ctx context.Context, args []any, kwargs map[string]any) (*modal.FunctionCall, error) {
	return f.spawnFn(ctx, args, kwargs)
}

// stubResolveModalFunction swaps the resolver seam to return fn, bypassing Modal.
func stubResolveModalFunction(t *testing.T, fn modalFunction) {
	orig := resolveModalFunctionFn
	resolveModalFunctionFn = func(context.Context, RequestContext, ComputeProviderConfig) (modalFunction, func(), error) {
		return fn, func() {}, nil
	}
	t.Cleanup(func() { resolveModalFunctionFn = orig })
}

// stubResolveModalFunctionError swaps the resolver seam to fail with err.
func stubResolveModalFunctionError(t *testing.T, err error) {
	orig := resolveModalFunctionFn
	resolveModalFunctionFn = func(context.Context, RequestContext, ComputeProviderConfig) (modalFunction, func(), error) {
		return nil, nil, err
	}
	t.Cleanup(func() { resolveModalFunctionFn = orig })
}

func TestModalInvokeWorker_Success_PassesIdentityKwargs(t *testing.T) {
	var gotKwargs map[string]any
	stubResolveModalFunction(t, &fakeModalFunction{
		spawnFn: func(_ context.Context, _ []any, kwargs map[string]any) (*modal.FunctionCall, error) {
			gotKwargs = kwargs
			return &modal.FunctionCall{}, nil
		},
	})

	p := &modalComputeProvider{}
	rc := RequestContext{NamespaceName: "ns", DeploymentName: "dep", DeploymentBuildID: "build-1"}

	require.NoError(t, p.InvokeWorker(t.Context(), rc, ComputeProviderConfig{}))
	assert.Equal(t, "ns", gotKwargs["namespace"])
	assert.Equal(t, "dep", gotKwargs["deployment_name"])
	assert.Equal(t, "build-1", gotKwargs["build_id"])
}

func TestModalInvokeWorker_SpawnError_Wrapped(t *testing.T) {
	sentinel := errors.New("boom")
	stubResolveModalFunction(t, &fakeModalFunction{
		spawnFn: func(context.Context, []any, map[string]any) (*modal.FunctionCall, error) {
			return nil, sentinel
		},
	})

	err := (&modalComputeProvider{}).InvokeWorker(t.Context(), RequestContext{}, ComputeProviderConfig{})
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
}

func TestModalInvokeWorker_ResolveError_Propagated(t *testing.T) {
	sentinel := errors.New("no client")
	stubResolveModalFunctionError(t, sentinel)

	err := (&modalComputeProvider{}).InvokeWorker(t.Context(), RequestContext{}, ComputeProviderConfig{})
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
}

func TestModalInvokeWorker_ClassifiesNotFound(t *testing.T) {
	stubResolveModalFunctionError(t, modal.NotFoundError{Exception: "missing"})

	err := (&modalComputeProvider{}).InvokeWorker(t.Context(), RequestContext{}, ComputeProviderConfig{})
	require.Error(t, err)
	var provErr *ProviderError
	require.ErrorAs(t, err, &provErr)
	assert.Equal(t, FailureNotFound, provErr.Class)
}

func TestModalUpdateWorkerSetSize_Unsupported(t *testing.T) {
	err := (&modalComputeProvider{}).UpdateWorkerSetSize(t.Context(), RequestContext{}, ComputeProviderConfig{}, 3)
	assert.ErrorIs(t, err, errors.ErrUnsupported)
}

func TestModalValidateConfig_Success(t *testing.T) {
	stubResolveModalFunction(t, &fakeModalFunction{})
	require.NoError(t, (&modalComputeProvider{}).ValidateConfig(t.Context(), RequestContext{}, ComputeProviderConfig{}))
}

func TestModalValidateConfig_ResolveError_Wrapped(t *testing.T) {
	sentinel := errors.New("not found")
	stubResolveModalFunctionError(t, sentinel)

	err := (&modalComputeProvider{}).ValidateConfig(t.Context(), RequestContext{}, ComputeProviderConfig{})
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
	assert.Contains(t, err.Error(), "cannot access the compute resource")
}

func TestModalTarget(t *testing.T) {
	// Deployment name -> app, build ID -> function.
	app, fn := modalTarget(RequestContext{NamespaceName: "default", DeploymentName: "modal-demo", DeploymentBuildID: "v1"})
	assert.Equal(t, "modal-demo", app)
	assert.Equal(t, "v1", fn)
}

func TestModalResolve_RequiresDeploymentAndBuild(t *testing.T) {
	// Uses the real resolver, which errors before creating a client when the WDV is incomplete.
	_, _, err := resolveModalFunction(t.Context(), RequestContext{}, ComputeProviderConfig{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deployment name and build ID")
}

func TestClassifyModalFailure(t *testing.T) {
	assert.Equal(t, FailureUnclassified, classifyModalFailure(nil))
	assert.Equal(t, FailureNotFound, classifyModalFailure(modal.NotFoundError{}))
	assert.Equal(t, FailureRejected, classifyModalFailure(modal.InvalidError{}))
	assert.Equal(t, FailureUnclassified, classifyModalFailure(errors.New("other")))
}
