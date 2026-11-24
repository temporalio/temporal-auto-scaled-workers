package modal

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	modalsdk "github.com/modal-labs/modal-client/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
)

// stressSource is a versioned grant store shared by many namespaces, safe for concurrent use. A
// grant's refresh token names its namespace, workspace and generation, so a spawn can check that
// it was handed its own grant.
type stressSource struct {
	mu          sync.Mutex
	generations map[credentialKey]int // absent means not connected
	failing     atomic.Bool

	reads    atomic.Int64
	versions atomic.Int64
}

func (s *stressSource) generation(key credentialKey) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.generations[key]
	return g, ok
}

func (s *stressSource) rotate(key credentialKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.generations[key]; ok {
		s.generations[key]++
	}
}

func (s *stressSource) Credentials(_ context.Context, namespace, workspaceID string) (*Credentials, error) {
	s.reads.Add(1)
	if s.failing.Load() {
		return nil, errors.New("grant storage unavailable")
	}
	key := credentialKey{namespace: namespace, workspaceID: workspaceID}
	g, ok := s.generation(key)
	if !ok {
		return nil, fmt.Errorf("%w: %s/%s", ErrCredentialsNotFound, namespace, workspaceID)
	}
	return &Credentials{
		OAuth: &OAuthCredentials{
			ClientID:     testModalClientID,
			JWTKey:       testModalJWTKey,
			RefreshToken: stressToken(key, g),
		},
		Version: strconv.Itoa(g),
	}, nil
}

func (s *stressSource) CredentialsVersion(_ context.Context, namespace, workspaceID string) (string, error) {
	s.versions.Add(1)
	if s.failing.Load() {
		return "", errors.New("grant storage unavailable")
	}
	key := credentialKey{namespace: namespace, workspaceID: workspaceID}
	g, ok := s.generation(key)
	if !ok {
		return "", fmt.Errorf("%w: %s/%s", ErrCredentialsNotFound, namespace, workspaceID)
	}
	return strconv.Itoa(g), nil
}

func stressToken(key credentialKey, generation int) string {
	return fmt.Sprintf("rt|%s|%s|%d", key.namespace, key.workspaceID, generation)
}

type stressKeyCtx struct{}

var stressClassNames = map[computeprovider.FailureClass]string{
	computeprovider.FailureUnclassified: "unclassified",
	computeprovider.FailureRejected:     "rejected",
	computeprovider.FailureNotFound:     "not_found",
	computeprovider.FailureAccessDenied: "access_denied",
	computeprovider.FailureUnavailable:  "unavailable",
	computeprovider.FailureThrottled:    "throttled",
	computeprovider.FailureInternal:     "internal",
	computeprovider.FailureConflict:     "conflict",
}

// stressClients stands in for Modal. A spawn checks the grant it was handed against the namespace
// and workspace that asked, rejects a grant storage has since replaced, and fails as a closed gRPC
// connection would once its client is closed.
type stressClients struct {
	source     *stressSource
	builds     atomic.Int64
	closes     atomic.Int64
	violations atomic.Int64
	firstErr   atomic.Pointer[string]
}

func (c *stressClients) firstViolation() string {
	if msg := c.firstErr.Load(); msg != nil {
		return *msg
	}
	return ""
}

func (c *stressClients) violate(format string, args ...any) {
	c.violations.Add(1)
	msg := fmt.Sprintf(format, args...)
	c.firstErr.CompareAndSwap(nil, &msg)
}

type stressClient struct {
	clients *stressClients
	token   string
	closed  atomic.Bool
}

func (c *stressClient) FunctionFromName(context.Context, string, string, *modalsdk.FunctionFromNameParams) (remoteFunction, error) {
	if c.closed.Load() {
		return nil, status.Error(codes.Canceled, "grpc: the client connection is closing")
	}
	return &stressFunction{client: c}, nil
}

func (c *stressClient) Close() {
	if c.closed.CompareAndSwap(false, true) {
		c.clients.closes.Add(1)
	}
}

type stressFunction struct{ client *stressClient }

func (f *stressFunction) Spawn(ctx context.Context, _ []any, _ map[string]any) error {
	if f.client.closed.Load() {
		return status.Error(codes.Canceled, "grpc: the client connection is closing")
	}
	want := ctx.Value(stressKeyCtx{}).(credentialKey)
	parts := strings.Split(f.client.token, "|")
	if len(parts) != 4 || parts[1] != want.namespace || parts[2] != want.workspaceID {
		f.client.clients.violate("%s/%s spawned with %q", want.namespace, want.workspaceID, f.client.token)
		return nil
	}
	used, _ := strconv.Atoi(parts[3])
	current, ok := f.client.clients.source.generation(want)
	if !ok || used > current {
		f.client.clients.violate("%s/%s spawned with generation %d, storage has %d (connected=%t)",
			want.namespace, want.workspaceID, used, current, ok)
		return nil
	}
	// A replaced grant is revoked, which sends WCI down its reload-after-rejection path.
	if used < current {
		return status.Error(codes.Unauthenticated, "grant revoked")
	}
	return nil
}

// TestModalCaches_StressAcrossNamespacesAndWorkspaces runs both caches through InvokeWorker from many
// goroutines at once, across namespaces that share workspace IDs, while one clock jumps far enough
// to trigger client sweeps, credential revalidation and day-long eviction, grants rotate, and grant
// storage fails and recovers. Run it with -race.
func TestModalCaches_StressAcrossNamespacesAndWorkspaces(t *testing.T) {
	const (
		namespaces = 20
		workspaces = 5
		workers    = 32
		epochs     = 40
		perEpoch   = 300
		maxAge     = time.Hour
	)

	clock := newAtomicClock()
	origCredClock, origClientClock := sharedCredentials.cacheClock, sharedClients.cacheClock
	sharedCredentials.cacheClock = cacheClock{now: clock.now, start: clock.now()}
	sharedClients.cacheClock = cacheClock{now: clock.now, start: clock.now()}
	t.Cleanup(func() {
		sharedCredentials.cacheClock, sharedClients.cacheClock = origCredClock, origClientClock
	})

	// Every namespace shares the same workspace IDs; each namespace leaves one of them unconnected.
	source := &stressSource{generations: map[credentialKey]int{}}
	var keys []credentialKey
	disconnected := map[credentialKey]bool{}
	for n := range namespaces {
		for w := range workspaces {
			key := credentialKey{namespace: fmt.Sprintf("ns%02d.acct%02d", n, n), workspaceID: fmt.Sprintf("ac-ws%d", w)}
			keys = append(keys, key)
			if w == n%workspaces {
				disconnected[key] = true
				continue
			}
			source.generations[key] = 1
		}
	}
	stubModalSecretSource(t, source)

	clients := &stressClients{source: source}
	orig := newClientFn
	newClientFn = func(creds *Credentials) (apiClient, error) {
		clients.builds.Add(1)
		return &stressClient{clients: clients, token: creds.OAuth.RefreshToken}, nil
	}
	t.Cleanup(func() { newClientFn = orig })

	provider := &functionProvider{
		credentialReloadPolicy: credentialPolicy{reloadLimit: 2, reloadCooldown: time.Minute, maxAge: maxAge},
		clientIdleTimeout:      30 * time.Minute,
	}
	spawn := func(key credentialKey) error {
		ctx := context.WithValue(t.Context(), stressKeyCtx{}, key)
		cfg := testModalConfig()
		cfg[configWorkspaceID] = key.workspaceID
		return provider.InvokeWorker(ctx, computeprovider.RequestContext{
			NamespaceName: key.namespace, DeploymentName: "dep", DeploymentBuildID: "build",
		}, cfg)
	}

	var (
		spawns, ok   atomic.Int64
		failuresMu   sync.Mutex
		failures     = map[string]int{}
		unexpectedMu sync.Mutex
		unexpected   []string
	)
	record := func(key credentialKey, err error) {
		spawns.Add(1)
		if err == nil {
			ok.Add(1)
			if disconnected[key] {
				unexpectedMu.Lock()
				unexpected = append(unexpected, fmt.Sprintf("%v spawned though it is not connected", key))
				unexpectedMu.Unlock()
			}
			return
		}
		var provErr *computeprovider.ProviderError
		class := computeprovider.FailureUnclassified
		if errors.As(err, &provErr) {
			class = provErr.Class
		}
		failuresMu.Lock()
		failures[stressClassNames[class]]++
		failuresMu.Unlock()

		// Storage failing, or a revoked grant that could not be replaced in time, is internal; an
		// unconnected workspace is not found; a spawn racing the close of a rotated client is
		// canceled and unclassified. Anything else is a bug.
		allowed := class == computeprovider.FailureInternal ||
			(class == computeprovider.FailureNotFound && disconnected[key]) ||
			(class == computeprovider.FailureUnclassified && status.Code(err) == codes.Canceled)
		if !allowed {
			unexpectedMu.Lock()
			unexpected = append(unexpected, fmt.Sprintf("%v: %v (class %v)", key, err, class))
			unexpectedMu.Unlock()
		}
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(i), 42))
			for {
				select {
				case <-done:
					return
				default:
				}
				key := keys[rng.IntN(len(keys))]
				record(key, spawn(key))
			}
		})
	}

	// Each epoch moves the clock past one of the thresholds, rotates some grants, and sometimes takes
	// storage down, then waits for the workers to make perEpoch more spawns.
	steps := []time.Duration{
		sweepInterval + time.Second,        // client sweep may run, nothing is idle yet
		31 * time.Minute,                   // client idle timeout passes
		maxAge + time.Minute,               // credentials are revalidated
		credentialEvictAfter + time.Minute, // credential entries are evicted
	}
	chaos := rand.New(rand.NewPCG(7, 7))
	for epoch := range epochs {
		clock.advance(steps[epoch%len(steps)])
		for range len(keys) / 10 {
			source.rotate(keys[chaos.IntN(len(keys))])
		}
		source.failing.Store(epoch%5 == 3)
		target := spawns.Load() + perEpoch
		for spawns.Load() < target {
			runtime.Gosched()
		}
	}
	close(done)
	wg.Wait()

	t.Logf("spawns=%d ok=%d failures=%v builds=%d closes=%d reads=%d version_checks=%d",
		spawns.Load(), ok.Load(), failures, clients.builds.Load(), clients.closes.Load(),
		source.reads.Load(), source.versions.Load())

	assert.Empty(t, unexpected, "only storage, not-connected and rotation-race failures are expected")
	// A client closed mid-spawn only happens when a grant rotates under it, which is rare; a cache
	// that replaced clients it should be reusing would cancel spawns constantly.
	assert.LessOrEqual(t, failures[stressClassNames[computeprovider.FailureUnclassified]], int(spawns.Load()/100),
		"too many spawns were canceled by their client closing")
	assert.Zero(t, clients.violations.Load(), "a spawn used another key's grant or one storage never held: %v", clients.firstViolation())
	require.Positive(t, clients.closes.Load(), "client sweeps never ran")
	require.Positive(t, source.versions.Load(), "credentials were never revalidated")
	require.Greater(t, source.reads.Load(), int64(len(keys)), "credentials were never re-read")

	// With storage healthy and a day gone, every key converges on what storage holds now.
	source.failing.Store(false)
	clock.advance(credentialEvictAfter + time.Minute)
	for _, key := range keys {
		err := spawn(key)
		if disconnected[key] {
			var provErr *computeprovider.ProviderError
			require.ErrorAs(t, err, &provErr)
			assert.Equal(t, computeprovider.FailureNotFound, provErr.Class, "%v", key)
			continue
		}
		assert.NoError(t, err, "%v", key)
	}
	assert.Zero(t, clients.violations.Load(), "after convergence: %v", clients.firstViolation())

	// Every client the run built is closed by the sweeps, the rotations or the reset.
	sharedClients.reset()
	assert.Equal(t, clients.builds.Load(), clients.closes.Load(), "a client was leaked")
}
