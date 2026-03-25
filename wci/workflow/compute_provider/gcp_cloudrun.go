package computeprovider

import (
	"context"
	"errors"
	"fmt"
	"math/rand"

	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"github.com/temporalio/temporal-auto-scaled-workers/wci/client"
	"github.com/temporalio/temporal-auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

const (
	configGCPCloudRunProject        = "project"
	configGCPCloudRunRegion         = "region"
	configGCPCloudRunWorkerPool     = "worker_pool"
	configGCPCloudRunServiceAccount = "service_account"
)

type gcpCloudRunComputeProvider struct {
	intermediaryServiceAccounts [][]client.GCPIAMServiceAccountRequest
}

func init() {
	RegisterComputeProvider(iface.ComputeProviderTypeGCPCloudRun, NewGCPCloudRunComputeProvider)
}

func NewGCPCloudRunComputeProvider(_ context.Context, dc *dynamicconfig.Collection) (ComputeProvider, error) {
	var intermediaryServiceAccounts [][]client.GCPIAMServiceAccountRequest
	if dc != nil {
		intermediaryServiceAccounts = client.WorkerControllerGCPIntermediaryServiceAccounts.Get(dc)()
	}

	return &gcpCloudRunComputeProvider{
		intermediaryServiceAccounts: intermediaryServiceAccounts,
	}, nil
}

func (p *gcpCloudRunComputeProvider) LaunchStrategy() LaunchStrategy {
	return LaunchStrategyWorkerSet
}

func (p *gcpCloudRunComputeProvider) ValidateConfig(ctx context.Context, config iface.ComputeProviderConfig) error {
	client, name, err := p.buildClientAndParams(ctx, config)
	if err != nil {
		return err
	}
	defer client.Close()
	_, err = client.GetWorkerPool(ctx, &runpb.GetWorkerPoolRequest{Name: name})
	if err != nil {
		return fmt.Errorf("worker pool %q not found: %w", name, err)
	}
	return nil
}

func (p *gcpCloudRunComputeProvider) InvokeWorker(ctx context.Context, config iface.ComputeProviderConfig) error {
	return errors.ErrUnsupported
}

func (p *gcpCloudRunComputeProvider) UpdateWorkerSetSize(ctx context.Context, config iface.ComputeProviderConfig, count int32) error {
	client, name, err := p.buildClientAndParams(ctx, config)
	if err != nil {
		return err
	}
	defer client.Close()

	op, err := client.UpdateWorkerPool(ctx, &runpb.UpdateWorkerPoolRequest{
		WorkerPool: &runpb.WorkerPool{
			Name:    name,
			Scaling: &runpb.WorkerPoolScaling{ManualInstanceCount: &count},
		},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"scaling"}},
	})
	if err != nil {
		return fmt.Errorf("failed to update worker pool %q: %w", name, err)
	}
	if _, err = op.Wait(ctx); err != nil {
		return fmt.Errorf("failed to wait for worker pool %q update: %w", name, err)
	}
	return nil
}

// buildClientAndParams creates a Cloud Run WorkerPoolsClient and constructs the fully-qualified worker pool name.
func (p *gcpCloudRunComputeProvider) buildClientAndParams(ctx context.Context, config iface.ComputeProviderConfig) (*run.WorkerPoolsClient, string, error) {
	project, ok := config[configGCPCloudRunProject].(string)
	if !ok || project == "" {
		return nil, "", fmt.Errorf("project not found in config")
	}
	region, ok := config[configGCPCloudRunRegion].(string)
	if !ok || region == "" {
		return nil, "", fmt.Errorf("region not found in config")
	}
	workerPool, ok := config[configGCPCloudRunWorkerPool].(string)
	if !ok || workerPool == "" {
		return nil, "", fmt.Errorf("worker_pool not found in config")
	}
	name := fmt.Sprintf("projects/%s/locations/%s/workerPools/%s", project, region, workerPool)

	var opts []option.ClientOption
	if serviceAccount, ok := config[configGCPCloudRunServiceAccount].(string); ok && serviceAccount != "" {
		delegates := []string{}
		for _, step := range p.intermediaryServiceAccounts {
			if len(step) == 0 {
				continue
			}

			req := step[rand.Intn(len(step))]
			if req.ServiceAccountEmail == "" {
				return nil, "", fmt.Errorf("invalid empty intermediary service account email")
			}

			delegates = append(delegates, req.ServiceAccountEmail)
		}

		ts, err := impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{
			TargetPrincipal: serviceAccount,
			Scopes:          []string{"https://www.googleapis.com/auth/cloud-platform"},
			Delegates:       delegates,
		})
		if err != nil {
			return nil, "", fmt.Errorf("failed to create impersonated credentials for %q: %w", serviceAccount, err)
		}
		opts = []option.ClientOption{option.WithTokenSource(ts)}
	}

	client, err := run.NewWorkerPoolsClient(ctx, opts...)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create Cloud Run client: %w", err)
	}
	return client, name, nil
}
