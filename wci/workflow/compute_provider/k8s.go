//go:build computeprovider_k8s && !release

package computeprovider

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	// The full generated clientset substantially increases compiled binary size:
	// "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
)

const (
	configK8sNamespace  = "namespace"
	configK8sDeployment = "deployment"
	configK8sKubeconfig = "kubeconfig"
	configK8sContext    = "context"
)

var k8sDeploymentsResource = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

type k8sComputeProvider struct{}

func init() {
	RegisterComputeProvider(iface.ComputeProviderTypeK8s, NewK8sComputeProvider)
}

func NewK8sComputeProvider(_ context.Context, _ *dynamicconfig.Collection) (ComputeProvider, error) {
	return &k8sComputeProvider{}, nil
}

func (p *k8sComputeProvider) LaunchStrategy() LaunchStrategy {
	return LaunchStrategyWorkerSet
}

func (p *k8sComputeProvider) ValidateConfig(ctx context.Context, _ RequestContext, config ComputeProviderConfig) error {
	client, namespace, deployment, err := p.buildClientAndParams(config)
	if err != nil {
		return err
	}
	_, err = client.Resource(k8sDeploymentsResource).Namespace(namespace).Get(ctx, deployment, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("deployment %q not found in namespace %q: %w", deployment, namespace, err)
	}
	return nil
}

func (p *k8sComputeProvider) InvokeWorker(ctx context.Context, _ RequestContext, config ComputeProviderConfig) error {
	return errors.ErrUnsupported
}

func (p *k8sComputeProvider) UpdateWorkerSetSize(ctx context.Context, _ RequestContext, config ComputeProviderConfig, count int32) error {
	client, namespace, deployment, err := p.buildClientAndParams(config)
	if err != nil {
		return err
	}

	deployments := client.Resource(k8sDeploymentsResource).Namespace(namespace)
	scale, err := deployments.Get(ctx, deployment, metav1.GetOptions{}, "scale")
	if err != nil {
		return fmt.Errorf("failed to get scale for deployment %q: %w", deployment, err)
	}

	if err := unstructured.SetNestedField(scale.Object, int64(count), "spec", "replicas"); err != nil {
		return fmt.Errorf("failed to set scale for deployment %q: %w", deployment, err)
	}
	_, err = deployments.Update(ctx, scale, metav1.UpdateOptions{}, "scale")
	if err != nil {
		return fmt.Errorf("failed to scale deployment %q to %d: %w", deployment, count, err)
	}
	return nil
}

// buildClientAndParams builds a Kubernetes client and extracts/validates namespace and deployment from config.
func (p *k8sComputeProvider) buildClientAndParams(config ComputeProviderConfig) (dynamic.Interface, string, string, error) {
	namespace, ok := config[configK8sNamespace].(string)
	if !ok || namespace == "" {
		return nil, "", "", fmt.Errorf("namespace not found in config")
	}
	deployment, ok := config[configK8sDeployment].(string)
	if !ok || deployment == "" {
		return nil, "", "", fmt.Errorf("deployment not found in config")
	}

	restConfig, err := p.buildRestConfig(config)
	if err != nil {
		return nil, "", "", err
	}

	client, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, "", "", fmt.Errorf("failed to create kubernetes client: %w", err)
	}
	return client, namespace, deployment, nil
}

// buildRestConfig returns a REST config using kubeconfig content (with optional context) or in-cluster config.
func (p *k8sComputeProvider) buildRestConfig(config ComputeProviderConfig) (*rest.Config, error) {
	kubeconfigContent, _ := config[configK8sKubeconfig].(string)

	if kubeconfigContent == "" {
		restConfig, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to build in-cluster config: %w", err)
		}
		return restConfig, nil
	}

	apiConfig, err := clientcmd.Load([]byte(kubeconfigContent))
	if err != nil {
		return nil, fmt.Errorf("failed to parse kubeconfig content: %w", err)
	}

	overrides := &clientcmd.ConfigOverrides{}
	if contextName, ok := config[configK8sContext].(string); ok && contextName != "" {
		overrides.CurrentContext = contextName
	}

	restConfig, err := clientcmd.NewDefaultClientConfig(*apiConfig, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to build REST config from kubeconfig content: %w", err)
	}
	return restConfig, nil
}
