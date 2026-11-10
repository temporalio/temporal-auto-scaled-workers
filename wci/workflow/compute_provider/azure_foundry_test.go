package computeprovider

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
)

type foundryCredentialFunc func(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error)

func (f foundryCredentialFunc) GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return f(ctx, opts)
}

type foundryTransportFunc func(*http.Request) (*http.Response, error)

func (f foundryTransportFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func foundryTestProvider() *azureFoundryComputeProvider {
	return &azureFoundryComputeProvider{
		credential: foundryCredentialFunc(func(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
			return azcore.AccessToken{Token: "test-token"}, nil
		}),
		client:     &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		maxRuntime: 50 * time.Millisecond,
	}
}
func foundryTestConfig() ComputeProviderConfig {
	return ComputeProviderConfig{"project_endpoint": "https://example.services.ai.azure.com/api/projects/test/", "agent_name": "my-worker"}
}

func TestAzureFoundryConfig(t *testing.T) {
	cases := []struct {
		key   string
		value any
	}{
		{"project_endpoint", nil}, {"project_endpoint", 123}, {"project_endpoint", "://"},
		{"project_endpoint", "http://host/api/projects/test"}, {"project_endpoint", "https://host"},
		{"project_endpoint", "https://user:pass@host/api/projects/test"},
		{"project_endpoint", "https://host/api/projects/test?q=x"}, {"project_endpoint", "https://host/api/projects/test?"},
		{"project_endpoint", "https://host/api/projects/test#x"},
		{"agent_name", nil}, {"agent_name", "-worker"}, {"agent_name", "worker-"}, {"agent_name", "a/b"},
		{"agent_name", "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijkl"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.key, "=", tc.value), func(t *testing.T) {
			cfg := foundryTestConfig()
			cfg[tc.key] = tc.value
			p := foundryTestProvider()
			p.credential = foundryCredentialFunc(func(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
				t.Fatal("invalid config requested credentials")
				return azcore.AccessToken{}, nil
			})
			require.Error(t, p.ValidateConfig(t.Context(), RequestContext{}, cfg))
			err := p.InvokeWorker(t.Context(), RequestContext{}, cfg)
			require.Equal(t, FailureRejected, classifyAzureFailure(err))
		})
	}
	p := foundryTestProvider()
	p.credential = foundryCredentialFunc(func(_ context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
		require.Equal(t, []string{azureFoundryScope}, opts.Scopes)
		return azcore.AccessToken{Token: "test-token"}, nil
	})
	p.client.Transport = foundryTransportFunc(func(*http.Request) (*http.Response, error) { t.Fatal("validation invoked a worker"); return nil, nil })
	require.NoError(t, p.ValidateConfig(t.Context(), RequestContext{}, foundryTestConfig()))
}

func TestAzureFoundryInvocation(t *testing.T) {
	p := foundryTestProvider()
	p.maxRuntime = time.Second
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/projects/test/agents/my-worker/endpoint/protocols/invocations?api-version=v1", r.URL.String())
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.Empty(t, r.Header.Get("x-agent-session-id"))
		var payload map[string]string
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		assert.Equal(t, map[string]string{"deploymentName": "deployment", "buildId": "build"}, payload)
		w.WriteHeader(http.StatusAccepted)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	p.client.Transport = server.Client().Transport
	cfg := foundryTestConfig()
	cfg["project_endpoint"] = server.URL + "/api/projects/test/"
	require.NoError(t, p.InvokeWorker(t.Context(), RequestContext{DeploymentName: "deployment", DeploymentBuildID: "build"}, cfg))
}

func TestAzureFoundryTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name           string
		sent           bool
		writeErr       error
		callerCanceled bool
		callerDeadline bool
		wantSuccess    bool
	}{
		{name: "handler slow", sent: true, wantSuccess: true},
		{name: "connection slow"},
		{name: "write incomplete", sent: true, writeErr: io.ErrUnexpectedEOF},
		{name: "caller canceled", sent: true, callerCanceled: true},
		{name: "caller deadline", sent: true, callerDeadline: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := foundryTestProvider()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.callerDeadline {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 10*time.Millisecond)
				defer stop()
			}
			p.client.Transport = foundryTransportFunc(func(r *http.Request) (*http.Response, error) {
				trace := httptrace.ContextClientTrace(r.Context())
				trace.GetConn("host")
				if tc.sent {
					trace.WroteRequest(httptrace.WroteRequestInfo{Err: tc.writeErr})
				}
				if tc.callerCanceled {
					cancel()
				}
				<-r.Context().Done()
				return nil, r.Context().Err()
			})
			err := p.InvokeWorker(ctx, RequestContext{}, foundryTestConfig())
			if tc.wantSuccess {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			if tc.callerCanceled {
				require.ErrorIs(t, err, context.Canceled)
			}
			if tc.callerDeadline {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			}
		})
	}
}

func TestAzureFoundryTLSFailure(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("untrusted TLS request reached handler") }))
	defer server.Close()
	p := foundryTestProvider()
	p.client.Transport = &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	cfg := foundryTestConfig()
	cfg["project_endpoint"] = server.URL + "/api/projects/test"
	require.Error(t, p.InvokeWorker(t.Context(), RequestContext{}, cfg))
}

type foundryTrackedBody struct{ closed bool }

func (*foundryTrackedBody) Read([]byte) (int, error) { return 0, errors.New("body must not be read") }
func (b *foundryTrackedBody) Close() error           { b.closed = true; return nil }

func TestAzureFoundryHTTPFailures(t *testing.T) {
	for _, tc := range []struct {
		status int
		class  FailureClass
	}{
		{200, FailureUnclassified}, {202, FailureUnclassified}, {204, FailureUnclassified},
		{302, FailureRejected}, {400, FailureRejected}, {401, FailureInternal}, {403, FailureAccessDenied},
		{404, FailureNotFound}, {408, FailureUnavailable}, {409, FailureConflict}, {424, FailureUnavailable},
		{429, FailureThrottled}, {500, FailureUnavailable}, {503, FailureUnavailable},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			p := foundryTestProvider()
			body := &foundryTrackedBody{}
			p.client.Transport = foundryTransportFunc(func(r *http.Request) (*http.Response, error) {
				require.Nil(t, r.GetBody)
				httptrace.ContextClientTrace(r.Context()).WroteRequest(httptrace.WroteRequestInfo{})
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Location": []string{"https://other.example/"}}, Body: body}, nil
			})
			err := p.InvokeWorker(t.Context(), RequestContext{}, foundryTestConfig())
			require.Equal(t, tc.class, classifyAzureFailure(err))
			require.True(t, body.closed)
			if tc.status >= 300 {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestAzureFoundryCredentials(t *testing.T) {
	for _, tc := range []struct {
		err   error
		class FailureClass
	}{
		{errors.New("no ambient credentials"), FailureInternal},
		{&azcore.ResponseError{StatusCode: 429}, FailureThrottled},
		{&azcore.ResponseError{StatusCode: 503}, FailureUnavailable},
		{context.Canceled, FailureUnclassified},
	} {
		p := foundryTestProvider()
		p.credential = foundryCredentialFunc(func(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
			return azcore.AccessToken{}, tc.err
		})
		err := p.InvokeWorker(t.Context(), RequestContext{}, foundryTestConfig())
		require.ErrorIs(t, err, tc.err)
		require.Equal(t, tc.class, classifyAzureFailure(err))
	}
}

func TestAzureFoundryRegistration(t *testing.T) {
	require.True(t, iface.ValidComputeProviderType("azure-foundry"))
	dc := dynamicconfig.NewCollection(dynamicconfig.StaticClient{
		client.WorkerControllerEnabledComputeProviders.Key(): []string{"azure-foundry"},
	}, log.NewNoopLogger())
	p, err := GetComputeProvider(t.Context(), iface.ComputeProviderTypeAzureFoundry, "test", dc)
	require.NoError(t, err)
	require.NotNil(t, p)
	require.Equal(t, LaunchStrategyInvoke, p.LaunchStrategy())
	require.ErrorIs(t, p.UpdateWorkerSetSize(t.Context(), RequestContext{}, nil, 1), errors.ErrUnsupported)
	dc = dynamicconfig.NewCollection(dynamicconfig.StaticClient(nil), log.NewNoopLogger())
	p, err = GetComputeProvider(t.Context(), iface.ComputeProviderTypeAzureFoundry, "test", dc)
	require.NoError(t, err)
	require.Nil(t, p)
}
