package computeprovider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/temporalio/temporal-managed-workers/wci/client"
	"github.com/temporalio/temporal-managed-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
)

const (
	configAWSECSCluster        = "cluster"
	configAWSECSService        = "service"
	configAWSECSRegion         = "region"
	configAWSECSRole           = "role"
	configAWSECSRoleExternalID = "role_external_id"
)

type awsECSComputeProvider struct {
	intermediaryRoles [][]client.AWSIAMRoleRequest
}

func init() {
	RegisterComputeProvider(iface.ComputeProviderTypeAWSECS, NewAWSECSComputeProvider)
}

func NewAWSECSComputeProvider(_ context.Context, dc *dynamicconfig.Collection) (ComputeProvider, error) {
	var intermediaryRoles [][]client.AWSIAMRoleRequest
	if dc != nil {
		intermediaryRoles = client.WorkerControllerAWSIntermediaryRoles.Get(dc)()
	}

	return &awsECSComputeProvider{
		intermediaryRoles: intermediaryRoles,
	}, nil
}

func (p *awsECSComputeProvider) LaunchStrategy() LaunchStrategy {
	return LaunchStrategyWorkerSet
}

func (p *awsECSComputeProvider) ValidateConfig(ctx context.Context, cfg iface.ComputeProviderConfig) error {
	ecsClient, cluster, service, err := p.getECSClientAndParams(ctx, cfg)
	if err != nil {
		return err
	}

	out, err := ecsClient.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster:  aws.String(cluster),
		Services: []string{service},
	})
	if err != nil {
		return fmt.Errorf("ECS DescribeServices failed: %w", err)
	}
	if len(out.Services) == 0 {
		if len(out.Failures) > 0 && out.Failures[0].Reason != nil {
			return fmt.Errorf("ECS service %q not found in cluster %q: %s", service, cluster, *out.Failures[0].Reason)
		}
		return fmt.Errorf("ECS service %q not found in cluster %q", service, cluster)
	}
	svc := out.Services[0]
	if svc.Status == nil || *svc.Status != "ACTIVE" {
		return fmt.Errorf("ECS service %q is not ACTIVE", service)
	}
	return nil
}

func (p *awsECSComputeProvider) InvokeWorker(ctx context.Context, cfg iface.ComputeProviderConfig) error {
	return errors.ErrUnsupported
}

func (p *awsECSComputeProvider) UpdateWorkerSetSize(ctx context.Context, cfg iface.ComputeProviderConfig, size int32) error {
	ecsClient, cluster, service, err := p.getECSClientAndParams(ctx, cfg)
	if err != nil {
		return err
	}

	_, err = ecsClient.UpdateService(ctx, &ecs.UpdateServiceInput{
		Cluster:      aws.String(cluster),
		Service:      aws.String(service),
		DesiredCount: aws.Int32(size),
	})
	if err != nil {
		return fmt.Errorf("ECS UpdateService failed: %w", err)
	}
	return nil
}

// getECSClientAndParams builds AWS config (including intermediary and config role assumption),
// returns an ECS client, cluster, and service.
func (p *awsECSComputeProvider) getECSClientAndParams(ctx context.Context, cfg iface.ComputeProviderConfig) (*ecs.Client, string, string, error) {
	cluster, ok := cfg[configAWSECSCluster].(string)
	if !ok || cluster == "" {
		return nil, "", "", fmt.Errorf("ECS cluster ARN or name not found or invalid")
	}

	service, ok := cfg[configAWSECSService].(string)
	if !ok || service == "" {
		return nil, "", "", fmt.Errorf("ECS service name or ARN not found or invalid")
	}

	// Resolve region: extract from cluster ARN, or fall back to "region" config key.
	var region string
	if strings.HasPrefix(cluster, "arn:") {
		var err error
		region, err = extractRegionFromARN(cluster)
		if err != nil {
			return nil, "", "", fmt.Errorf("failed to extract region from cluster ARN: %w", err)
		}
	} else {
		r, ok := cfg[configAWSECSRegion].(string)
		if !ok || r == "" {
			return nil, "", "", fmt.Errorf("region must be specified when cluster is not an ARN")
		}
		region = r
	}

	roleARN, _ := cfg[configAWSECSRole].(string)
	if roleARN != "" {
		if err := validateRoleARN(roleARN); err != nil {
			return nil, "", "", err
		}
	}

	var roleExternalID *string
	if eid, ok := cfg[configAWSECSRoleExternalID].(string); ok && eid != "" {
		roleExternalID = &eid
	}

	awsConfig, err := buildAWSConfig(ctx, region, roleARN, roleExternalID, p.intermediaryRoles)
	if err != nil {
		return nil, "", "", err
	}

	return ecs.NewFromConfig(awsConfig), cluster, service, nil
}
