//go:build !release

package computeprovider

import (
	"context"
	"errors"
	"fmt"

	"github.com/temporalio/temporal-auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	configK8sNamespace  = "namespace"
	configK8sDeployment = "deployment"
	configK8sKubeconfig = "kubeconfig"
	configK8sContext    = "context"
)

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

func (p *k8sComputeProvider) ValidateConfig(ctx context.Context, config iface.ComputeProviderConfig) error {
	client, namespace, deployment, err := p.buildClientAndParams(config)
	if err != nil {
		return err
	}
	_, err = client.AppsV1().Deployments(namespace).Get(ctx, deployment, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("deployment %q not found in namespace %q: %w", deployment, namespace, err)
	}
	return nil
}

func (p *k8sComputeProvider) InvokeWorker(ctx context.Context, config iface.ComputeProviderConfig) error {
	return errors.ErrUnsupported
}

func (p *k8sComputeProvider) UpdateWorkerSetSize(ctx context.Context, config iface.ComputeProviderConfig, count int32) error {
	client, namespace, deployment, err := p.buildClientAndParams(config)
	if err != nil {
		return err
	}

	scale, err := client.AppsV1().Deployments(namespace).GetScale(ctx, deployment, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get scale for deployment %q: %w", deployment, err)
	}

	scale.Spec.Replicas = count
	_, err = client.AppsV1().Deployments(namespace).UpdateScale(ctx, deployment, scale, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to scale deployment %q to %d: %w", deployment, count, err)
	}
	return nil
}

// buildClientAndParams builds a Kubernetes client and extracts/validates namespace and deployment from config.
func (p *k8sComputeProvider) buildClientAndParams(config iface.ComputeProviderConfig) (kubernetes.Interface, string, string, error) {
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

	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, "", "", fmt.Errorf("failed to create kubernetes client: %w", err)
	}
	return client, namespace, deployment, nil
}

// buildRestConfig returns a REST config using kubeconfig content (with optional context) or in-cluster config.
func (p *k8sComputeProvider) buildRestConfig(config iface.ComputeProviderConfig) (*rest.Config, error) {
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
