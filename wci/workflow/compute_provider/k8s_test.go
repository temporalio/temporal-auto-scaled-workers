//go:build computeprovider_k8s && !release

package computeprovider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestK8sUpdateWorkerSetSize(t *testing.T) {
	const (
		scalePath             = "/apis/apps/v1/namespaces/test-namespace/deployments/test-deployment/scale"
		desiredReplicas int32 = 3
	)
	var actualReplicas atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != scalePath || (r.Method != http.MethodGet && r.Method != http.MethodPut) {
			http.NotFound(w, r)
			return
		}

		scale := autoscalingv1.Scale{
			TypeMeta:   metav1.TypeMeta{APIVersion: "autoscaling/v1", Kind: "Scale"},
			ObjectMeta: metav1.ObjectMeta{Name: "test-deployment"},
		}
		if r.Method == http.MethodPut {
			if err := json.NewDecoder(r.Body).Decode(&scale); err != nil {
				http.Error(w, "invalid scale update", http.StatusBadRequest)
				return
			}
			actualReplicas.Store(scale.Spec.Replicas)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(scale)
	}))
	t.Cleanup(server.Close)

	kubeconfig, err := clientcmd.Write(clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{"test": {Server: server.URL}},
		Contexts:       map[string]*clientcmdapi.Context{"test": {Cluster: "test"}},
		CurrentContext: "test",
	})
	require.NoError(t, err)
	require.NoError(t, (&k8sComputeProvider{}).UpdateWorkerSetSize(t.Context(), RequestContext{}, ComputeProviderConfig{
		configK8sNamespace:  "test-namespace",
		configK8sDeployment: "test-deployment",
		configK8sKubeconfig: string(kubeconfig),
	}, desiredReplicas))
	require.Equal(t, desiredReplicas, actualReplicas.Load())
}
