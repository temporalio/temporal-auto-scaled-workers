//go:build test_dep

package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	modal "github.com/modal-labs/modal-client/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonpb "go.temporal.io/api/common/v1"
	computepb "go.temporal.io/api/compute/v1"
	deploymentpb "go.temporal.io/api/deployment/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/auto-scaled-workers/wci/client"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/sdk"
	"go.temporal.io/server/tests/testcore"
)

const (
	modalTestAccount     = "integration-acct"
	modalTestWorkspace   = "ws-integration"
	modalTestApp         = "temporal-workers"
	modalTestFunction    = "spawn_worker"
	modalTestEnvironment = "main"
	modalTestClientID    = "oc-integration"
	modalTestJWTKey      = "-----BEGIN RSA PRIVATE KEY-----\nintegration\n-----END RSA PRIVATE KEY-----"
)

// createModalTestEnv mirrors createWCITestEnv but additionally enables the
// modal compute provider, which the other scenarios do not need.
func createModalTestEnv(t *testing.T) *testcore.TestEnv {
	t.Helper()

	return testcore.NewEnv(t,
		testcore.WithDynamicConfig(client.WorkerControllerEnabled, true),
		testcore.WithDynamicConfig(client.WorkerControllerEnabledComputeProviders, []string{
			string(iface.ComputeProviderTypeModalFunction),
		}),
		testcore.WithDynamicConfig(dynamicconfig.VersionDrainageStatusRefreshInterval, testVersionDrainageRefreshInterval),
		testcore.WithDynamicConfig(dynamicconfig.VersionDrainageStatusVisibilityGracePeriod, testVersionDrainageVisibilityGracePeriod),
		testcore.WithDynamicConfig(client.WorkerControllerMinSignalIntervalNoSyncMatchMilliseconds, 1),
	)
}

// modalFakeSecrets is an in-memory stand-in for Vault. It records reads so a
// test can assert that secret storage stays off the per-invocation path.
type modalFakeSecrets struct {
	mu sync.Mutex

	jwtKey string
	// grants maps "<account>/<workspace>" to a refresh token. A grant that is
	// absent is a workspace the account has not connected.
	grants map[string]string

	signingKeyReads   int
	refreshTokenReads int
}

func newModalFakeSecrets() *modalFakeSecrets {
	return &modalFakeSecrets{
		jwtKey: modalTestJWTKey,
		grants: map[string]string{modalTestAccount + "/" + modalTestWorkspace: "rt-integration"},
	}
}

func (s *modalFakeSecrets) Credentials(_ context.Context, accountID, workspaceID string) (*computeprovider.ModalCredentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signingKeyReads++
	s.refreshTokenReads++

	token, ok := s.grants[accountID+"/"+workspaceID]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no grant for %s on %s", accountID, workspaceID)
	}
	return &computeprovider.ModalCredentials{OAuth: &computeprovider.ModalOAuthCredentials{
		ClientID:     modalTestClientID,
		JWTKey:       s.jwtKey,
		RefreshToken: token,
	}}, nil
}

func (s *modalFakeSecrets) reads() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.signingKeyReads, s.refreshTokenReads
}

// modalSpy records what the provider asked the fake Modal control plane to do.
type modalSpy struct {
	mu sync.Mutex

	spawns    []string
	lookups   []string
	credsSeen []computeprovider.ModalCredentials

	events chan string
}

func newModalSpy() *modalSpy {
	return &modalSpy{events: make(chan string, 32)}
}

func (s *modalSpy) recordLookup(appName, functionName string, creds computeprovider.ModalCredentials) {
	s.mu.Lock()
	s.lookups = append(s.lookups, appName+"/"+functionName)
	s.credsSeen = append(s.credsSeen, creds)
	s.mu.Unlock()
	s.emit("lookup")
}

func (s *modalSpy) recordSpawn(appName, functionName string) {
	s.mu.Lock()
	s.spawns = append(s.spawns, appName+"/"+functionName)
	s.mu.Unlock()
	s.emit("invoke")
}

func (s *modalSpy) emit(action string) {
	select {
	case s.events <- action:
	default:
	}
}

func (s *modalSpy) spawnCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.spawns)
}

func (s *modalSpy) lookupCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.lookups)
}

func (s *modalSpy) credentials() []computeprovider.ModalCredentials {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]computeprovider.ModalCredentials(nil), s.credsSeen...)
}

// installModalFakes points the Modal provider at an in-memory control plane and
// secret store, and derives every namespace to modalTestAccount so the test
// does not depend on the cluster's namespace naming.
func installModalFakes(t *testing.T, secrets computeprovider.ModalSecretSource, spy *modalSpy, spawnErr func() error) {
	t.Helper()

	t.Cleanup(computeprovider.SetModalTestSecretSource(secrets))
	t.Cleanup(computeprovider.SetModalTestClient(computeprovider.ModalTestClientConfig{
		LookupFn: func(_ context.Context, appName, functionName string, _ *modal.FunctionFromNameParams, creds computeprovider.ModalCredentials) error {
			spy.recordLookup(appName, functionName, creds)
			return nil
		},
		Function: computeprovider.ModalTestFunction{
			SpawnFn: func(_ context.Context, appName, functionName string) error {
				if spawnErr != nil {
					if err := spawnErr(); err != nil {
						return err
					}
				}
				spy.recordSpawn(appName, functionName)
				return nil
			},
		},
	}))

	orig := modalAccountResolverForTest()
	computeprovider.SetModalAccountResolver(fixedAccountResolver{accountID: modalTestAccount})
	t.Cleanup(func() { computeprovider.SetModalAccountResolver(orig) })
}

// modalAccountResolverForTest returns the resolver to restore after a test. The
// default is the namespace-suffix one the service installs at startup.
func modalAccountResolverForTest() computeprovider.ModalAccountResolver {
	return computeprovider.NamespaceSuffixModalAccountResolver{}
}

type fixedAccountResolver struct{ accountID string }

func (r fixedAccountResolver) ResolveAccountID(context.Context, string) (string, error) {
	return r.accountID, nil
}

func modalComputeConfig(overrides map[string]string) *computepb.ComputeConfig {
	details := map[string]string{
		"app_name":      modalTestApp,
		"function_name": modalTestFunction,
		"environment":   modalTestEnvironment,
		"workspace_id":  modalTestWorkspace,
	}
	for k, v := range overrides {
		if v == "" {
			delete(details, k)
			continue
		}
		details[k] = v
	}

	payload, err := sdk.PreferProtoDataConverter.ToPayload(details)
	if err != nil {
		panic(err)
	}

	return &computepb.ComputeConfig{
		ScalingGroups: map[string]*computepb.ComputeConfigScalingGroup{
			"default": {
				Provider: &computepb.ComputeProvider{
					Type:    "modal-function",
					Details: payload,
				},
				Scaler: &computepb.ComputeScaler{
					Type:    "no-sync",
					Details: modalScalerDetails(),
				},
			},
		},
	}
}

func modalScalerDetails() *commonpb.Payload {
	payload, err := sdk.PreferProtoDataConverter.ToPayload(map[string]string{
		"scale_up_backlog_threshold": "0",
		"scale_up_cooloff_ms":        "500",
		"max_worker_lifetime_ms":     "300000",
	})
	if err != nil {
		panic(err)
	}
	return payload
}

func createModalVersion(t *testing.T, ctx context.Context, cli sdkclient.Client, namespace string, version *deploymentpb.WorkerDeploymentVersion, cfg *computepb.ComputeConfig) error {
	t.Helper()
	_, err := cli.WorkflowService().CreateWorkerDeploymentVersion(ctx,
		&workflowservice.CreateWorkerDeploymentVersionRequest{
			Namespace:         namespace,
			DeploymentVersion: version,
			Identity:          "test-identity",
			ComputeConfig:     cfg,
			RequestId:         uuid.NewString(),
		})
	return err
}

// A Modal-backed version spawns a worker to register its task queues and again
// when a backlog forms, reading the grant from secret storage only once.
func testWCIModalScaleUp(t *testing.T) {
	env := createModalTestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	namespace := env.Namespace().String()
	deploymentName := uuid.NewString()
	buildID := uuid.NewString()
	taskQueue := "modal-tq-" + deploymentName

	secrets := newModalFakeSecrets()
	spy := newModalSpy()
	installModalFakes(t, secrets, spy, nil)

	createWorkerDeployment(t, env, deploymentName)

	version := &deploymentpb.WorkerDeploymentVersion{DeploymentName: deploymentName, BuildId: buildID}
	require.NoError(t, createModalVersion(t, ctx, cli, namespace, version, modalComputeConfig(nil)))

	// Validating the spec looks the Function up; registering the task queues then
	// spawns one.
	waitForInvoke(t, spy.events, 60*time.Second, "register-task-queues spawn")

	registerVersionTaskQueue(t, ctx, cli, namespace, deploymentName, buildID, taskQueue)
	drainEvents(t, spy.events)
	spawnsBeforeBacklog := spy.spawnCount()

	run := submitPinnedWorkflow(t, ctx, cli, taskQueue, deploymentName, buildID)
	waitForInvoke(t, spy.events, 60*time.Second, "scale-up spawn")
	assert.Greater(t, spy.spawnCount(), spawnsBeforeBacklog)

	// Every spawn authenticated as the grant resolved for the namespace's account.
	for _, creds := range spy.credentials() {
		require.NotNil(t, creds.OAuth, "per-account mode must supply grant material")
		assert.Equal(t, "rt-integration", creds.OAuth.RefreshToken)
		assert.Equal(t, modalTestClientID, creds.OAuth.ClientID)
		assert.Equal(t, modalTestJWTKey, creds.OAuth.JWTKey)
	}

	signingReads, grantReads := secrets.reads()
	assert.Equal(t, 1, signingReads, "secret storage must be read once for the process, not per spawn")
	assert.Equal(t, 1, grantReads)

	// Validating the version resolved the Function; every spawn since has reused
	// that handle and its connection rather than paying for a lookup of its own.
	assert.Equal(t, 1, spy.lookupCount(), "the Function handle must be resolved once, not per spawn")
	assert.Greater(t, spy.spawnCount(), 1)

	w := startVersionedWorker(t, cli, taskQueue, deploymentName, buildID)
	t.Cleanup(w.Stop)

	var result string
	getCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	require.NoError(t, run.Get(getCtx, &result))
	require.Equal(t, "foo", result)
}

// A version naming a workspace the account has no grant on is rejected at
// creation rather than failing later at invocation time.
func testWCIModalCreateVersionWithoutGrant(t *testing.T) {
	env := createModalTestEnv(t)

	secrets := newModalFakeSecrets()
	spy := newModalSpy()
	installModalFakes(t, secrets, spy, nil)

	deploymentName := uuid.NewString()
	createWorkerDeployment(t, env, deploymentName)

	version := &deploymentpb.WorkerDeploymentVersion{DeploymentName: deploymentName, BuildId: uuid.NewString()}
	cfg := modalComputeConfig(map[string]string{"workspace_id": "ws-not-connected"})

	err := createModalVersion(t, env.Context(), env.SdkClient(), env.Namespace().String(), version, cfg)
	require.Error(t, err)
	assert.Zero(t, spy.spawnCount(), "no worker may be spawned without a resolved grant")
}

// A config missing a required field is rejected at creation.
func testWCIModalCreateVersionInvalidConfig(t *testing.T) {
	env := createModalTestEnv(t)

	secrets := newModalFakeSecrets()
	spy := newModalSpy()
	installModalFakes(t, secrets, spy, nil)

	deploymentName := uuid.NewString()
	createWorkerDeployment(t, env, deploymentName)

	version := &deploymentpb.WorkerDeploymentVersion{DeploymentName: deploymentName, BuildId: uuid.NewString()}
	cfg := modalComputeConfig(map[string]string{"function_name": ""})

	err := createModalVersion(t, env.Context(), env.SdkClient(), env.Namespace().String(), version, cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "function_name")
	assert.Zero(t, spy.spawnCount())
}

// A Modal Function that cannot be looked up fails version creation with the
// provider's own message, so the operator sees what Modal said.
func testWCIModalCreateVersionFunctionNotFound(t *testing.T) {
	env := createModalTestEnv(t)

	secrets := newModalFakeSecrets()
	spy := newModalSpy()

	t.Cleanup(computeprovider.SetModalTestSecretSource(secrets))
	t.Cleanup(computeprovider.SetModalTestClient(computeprovider.ModalTestClientConfig{
		LookupFn: func(context.Context, string, string, *modal.FunctionFromNameParams, computeprovider.ModalCredentials) error {
			return modal.NotFoundError{Exception: "Function 'temporal-workers/spawn_worker' not found"}
		},
	}))
	orig := modalAccountResolverForTest()
	computeprovider.SetModalAccountResolver(fixedAccountResolver{accountID: modalTestAccount})
	t.Cleanup(func() { computeprovider.SetModalAccountResolver(orig) })

	deploymentName := uuid.NewString()
	createWorkerDeployment(t, env, deploymentName)

	version := &deploymentpb.WorkerDeploymentVersion{DeploymentName: deploymentName, BuildId: uuid.NewString()}
	err := createModalVersion(t, env.Context(), env.SdkClient(), env.Namespace().String(), version, modalComputeConfig(nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	assert.Zero(t, spy.spawnCount())
}

// With no secret source installed -- the open-source default -- a version works
// without naming a workspace, and the Modal SDK is left to find the one API key
// configured for the process.
func testWCIModalAPIKeyCredentials(t *testing.T) {
	env := createModalTestEnv(t)
	ctx := env.Context()
	cli := env.SdkClient()

	spy := newModalSpy()
	t.Cleanup(computeprovider.SetModalTestClient(computeprovider.ModalTestClientConfig{
		LookupFn: func(_ context.Context, appName, functionName string, _ *modal.FunctionFromNameParams, creds computeprovider.ModalCredentials) error {
			spy.recordLookup(appName, functionName, creds)
			return nil
		},
		Function: computeprovider.ModalTestFunction{
			SpawnFn: func(_ context.Context, appName, functionName string) error {
				spy.recordSpawn(appName, functionName)
				return nil
			},
		},
	}))

	deploymentName := uuid.NewString()
	createWorkerDeployment(t, env, deploymentName)

	version := &deploymentpb.WorkerDeploymentVersion{DeploymentName: deploymentName, BuildId: uuid.NewString()}
	cfg := modalComputeConfig(map[string]string{"workspace_id": ""})

	require.NoError(t, createModalVersion(t, ctx, cli, env.Namespace().String(), version, cfg))

	waitForInvoke(t, spy.events, 60*time.Second, "register-task-queues spawn")
	for _, creds := range spy.credentials() {
		assert.True(t, creds.APIKey, "the SDK should be left to find its own API key")
		assert.Nil(t, creds.OAuth, "no grant material should be supplied")
	}
}
