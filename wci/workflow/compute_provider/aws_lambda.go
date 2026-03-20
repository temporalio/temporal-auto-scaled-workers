package computeprovider

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/temporalio/temporal-managed-workers/wci/client"
	"github.com/temporalio/temporal-managed-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
)

const (
	configAWSLambdaARN            = "arn"
	configAWSLambdaRole           = "role"
	configAWSLambdaRoleExternalID = "role_external_id"
)

type awsLambdaComputeProvider struct {
	intermediaryRoles          [][]client.AWSIAMRoleRequest
	requireRoleAndExternalID   bool
}

func init() {
	RegisterComputeProvider(iface.ComputeProviderTypeAWSLambda, NewAWSLambdaComputeProvider)
}

func NewAWSLambdaComputeProvider(_ context.Context, dc *dynamicconfig.Collection) (ComputeProvider, error) {
	var intermediaryRoles [][]client.AWSIAMRoleRequest
	requireRoleAndExternalID := true
	if dc != nil {
		intermediaryRoles = client.WorkerControllerAWSIntermediaryRoles.Get(dc)()
		requireRoleAndExternalID = client.WorkerControllerAWSRequireRoleAndExternalID.Get(dc)()
	}

	return &awsLambdaComputeProvider{
		intermediaryRoles:        intermediaryRoles,
		requireRoleAndExternalID: requireRoleAndExternalID,
	}, nil
}

func (p *awsLambdaComputeProvider) LaunchStrategy() LaunchStrategy {
	return LaunchStrategyInvoke
}

func (p *awsLambdaComputeProvider) ValidateConfig(ctx context.Context, cfg iface.ComputeProviderConfig) error {
	if p.requireRoleAndExternalID {
		if roleARN, _ := cfg[configAWSLambdaRole].(string); roleARN == "" {
			return fmt.Errorf("AWS Lambda compute provider requires %q to be configured", configAWSLambdaRole)
		}
		if eid, _ := cfg[configAWSLambdaRoleExternalID].(string); eid == "" {
			return fmt.Errorf("AWS Lambda compute provider requires %q to be configured", configAWSLambdaRoleExternalID)
		}
	}

	lambdaClient, arn, err := p.getLambdaClientAndARN(ctx, cfg)
	if err != nil {
		return err
	}

	if _, err = lambdaClient.GetFunction(ctx, &lambda.GetFunctionInput{
		FunctionName: aws.String(arn),
	}); err != nil {
		return fmt.Errorf("lambda GetFunction failed: %w", err)
	}

	if err := p.checkExternalID(ctx, cfg, arn); err != nil {
		return err
	}

	return nil
}

func (p *awsLambdaComputeProvider) InvokeWorker(ctx context.Context, cfg iface.ComputeProviderConfig) error {
	lambdaClient, arn, err := p.getLambdaClientAndARN(ctx, cfg)
	if err != nil {
		return err
	}

	resp, err := lambdaClient.Invoke(ctx, &lambda.InvokeInput{
		FunctionName:   aws.String(arn),
		InvocationType: types.InvocationTypeEvent,
	})
	if err != nil {
		return fmt.Errorf("failed to invoke lambda: %w", err)
	}

	if resp.FunctionError != nil {
		return fmt.Errorf("failed to invoke lambda: %s", *resp.FunctionError)
	}

	return nil
}

func (p *awsLambdaComputeProvider) UpdateWorkerSetSize(_ context.Context, _ iface.ComputeProviderConfig, _ int32) error {
	return errors.ErrUnsupported
}

func (p *awsLambdaComputeProvider) checkExternalID(ctx context.Context, cfg iface.ComputeProviderConfig, arn string) error {
	roleARN, _ := cfg[configAWSLambdaRole].(string)
	eid, _ := cfg[configAWSLambdaRoleExternalID].(string)
	if roleARN == "" || eid == "" {
		return nil
	}
	region, err := extractRegionFromARN(arn)
	if err != nil {
		return fmt.Errorf("cannot verify external ID enforcement: failed to extract region from Lambda ARN %q: %w", arn, err)
	}
	return verifyExternalIDEnforcedFn(ctx, region, roleARN, p.intermediaryRoles)
}

// getLambdaClientAndARN builds AWS config (including intermediary and config role assumption), returns a Lambda client and the function ARN.
func (p *awsLambdaComputeProvider) getLambdaClientAndARN(ctx context.Context, cfg iface.ComputeProviderConfig) (*lambda.Client, string, error) {
	arn, ok := cfg[configAWSLambdaARN].(string)
	if !ok || arn == "" {
		return nil, "", fmt.Errorf("AWS Lambda Function ARN not found or invalid")
	}

	region, err := extractRegionFromARN(arn)
	if err != nil {
		return nil, "", err
	}

	roleARN, _ := cfg[configAWSLambdaRole].(string)
	if roleARN != "" {
		if err := validateRoleARN(roleARN); err != nil {
			return nil, "", err
		}
	}

	var roleExternalID *string
	if eid, ok := cfg[configAWSLambdaRoleExternalID].(string); ok && eid != "" {
		roleExternalID = &eid
	}

	awsConfig, err := buildAWSConfig(ctx, region, roleARN, roleExternalID, p.intermediaryRoles)
	if err != nil {
		return nil, "", err
	}

	return lambda.NewFromConfig(awsConfig), arn, nil
}
