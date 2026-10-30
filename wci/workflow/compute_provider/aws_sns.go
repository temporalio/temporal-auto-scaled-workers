package computeprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/service/sns"

	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
)

const (
	configAWSSNSTopicARN       = "topic_arn"
	configAWSSNSRole           = "role"
	configAWSSNSRoleExternalID = "role_external_id"

	// AWSSNSWorkerActivationEvent is the event value published for worker activation.
	AWSSNSWorkerActivationEvent = "worker-activation"
)

// AWSSNSWorkerActivationMessage is the JSON payload published to the configured SNS
// topic when the worker controller requests worker activation. This exported type is
// the public contract for SNS subscribers and can be used to generate a JSON Schema.
type AWSSNSWorkerActivationMessage struct {
	Event          string `json:"event"`
	NamespaceName  string `json:"namespaceName"`
	DeploymentName string `json:"deploymentName"`
	BuildID        string `json:"buildId"`
}

type awsSNSComputeProvider struct {
	intermediaryRoles [][]client.AWSIAMRoleRequest
}

type snsAPI interface {
	Publish(context.Context, *sns.PublishInput, ...func(*sns.Options)) (*sns.PublishOutput, error)
	GetTopicAttributes(context.Context, *sns.GetTopicAttributesInput, ...func(*sns.Options)) (*sns.GetTopicAttributesOutput, error)
}

func init() {
	RegisterComputeProvider(iface.ComputeProviderTypeAWSSNS, NewAWSSNSComputeProvider)
}

func NewAWSSNSComputeProvider(_ context.Context, dc *dynamicconfig.Collection) (ComputeProvider, error) {
	var intermediaryRoles [][]client.AWSIAMRoleRequest
	if dc != nil {
		intermediaryRoles = client.WorkerControllerAWSIntermediaryRoles.Get(dc)()
	}
	return &awsSNSComputeProvider{intermediaryRoles: intermediaryRoles}, nil
}

func (p *awsSNSComputeProvider) LaunchStrategy() LaunchStrategy {
	return LaunchStrategyInvoke
}

func (p *awsSNSComputeProvider) ValidateConfig(ctx context.Context, _ RequestContext, cfg ComputeProviderConfig) error {
	snsClient, topicARN, region, err := p.getSNSClientAndTopicARN(ctx, cfg)
	if err != nil {
		return fmt.Errorf("cannot connect to the compute provider: %w", err)
	}
	if _, err := snsClient.GetTopicAttributes(ctx, &sns.GetTopicAttributesInput{
		TopicArn: aws.String(topicARN),
	}); err != nil {
		return fmt.Errorf("cannot access the compute resource: %w", err)
	}
	if err := p.checkExternalID(ctx, cfg, region); err != nil {
		return fmt.Errorf("IAM role trust policy does not enforce ExternalID condition: %w", err)
	}
	return nil
}

func (p *awsSNSComputeProvider) InvokeWorker(ctx context.Context, rc RequestContext, cfg ComputeProviderConfig) error {
	err := p.invokeWorker(ctx, rc, cfg)
	return NewProviderError(classifyAWSFailure(err), err)
}

func (p *awsSNSComputeProvider) invokeWorker(ctx context.Context, rc RequestContext, cfg ComputeProviderConfig) error {
	snsClient, topicARN, _, err := p.getSNSClientAndTopicARN(ctx, cfg)
	if err != nil {
		return err
	}

	message, err := json.Marshal(AWSSNSWorkerActivationMessage{
		Event:          AWSSNSWorkerActivationEvent,
		NamespaceName:  rc.NamespaceName,
		DeploymentName: rc.DeploymentName,
		BuildID:        rc.DeploymentBuildID,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal SNS worker activation message: %w", err)
	}

	if _, err := snsClient.Publish(ctx, &sns.PublishInput{
		TopicArn: aws.String(topicARN),
		Message:  aws.String(string(message)),
	}); err != nil {
		return fmt.Errorf("failed to publish worker activation message to SNS topic %q: %w", topicARN, err)
	}
	return nil
}

func (p *awsSNSComputeProvider) UpdateWorkerSetSize(_ context.Context, _ RequestContext, _ ComputeProviderConfig, _ int32) error {
	return errors.ErrUnsupported
}

func (p *awsSNSComputeProvider) checkExternalID(ctx context.Context, cfg ComputeProviderConfig, region string) error {
	roleARN, _ := cfg[configAWSSNSRole].(string)
	externalID, _ := cfg[configAWSSNSRoleExternalID].(string)
	if roleARN == "" || externalID == "" {
		return nil
	}
	return verifyExternalIDEnforcedFn(ctx, region, roleARN, p.intermediaryRoles)
}

var newSNSClientFn = newSNSClient

func newSNSClient(ctx context.Context, region, roleARN string, externalID *string, intermediaryRoles [][]client.AWSIAMRoleRequest) (snsAPI, error) {
	awsConfig, err := buildAWSConfig(ctx, region, roleARN, externalID, intermediaryRoles)
	if err != nil {
		return nil, err
	}
	return sns.NewFromConfig(awsConfig), nil
}

func (p *awsSNSComputeProvider) getSNSClientAndTopicARN(ctx context.Context, cfg ComputeProviderConfig) (snsAPI, string, string, error) {
	topicARN, ok := cfg[configAWSSNSTopicARN].(string)
	if !ok || topicARN == "" {
		return nil, "", "", fmt.Errorf("AWS SNS topic ARN not found or invalid")
	}

	parsed, err := arn.Parse(topicARN)
	if err != nil {
		return nil, "", "", fmt.Errorf("invalid SNS topic ARN %q: %w", topicARN, err)
	}
	if parsed.Service != "sns" || parsed.Region == "" || parsed.AccountID == "" || parsed.Resource == "" {
		return nil, "", "", fmt.Errorf("AWS SNS topic ARN must reference an SNS topic")
	}

	roleARN, _ := cfg[configAWSSNSRole].(string)
	if roleARN != "" {
		if err := validateRoleARN(roleARN); err != nil {
			return nil, "", "", err
		}
	}

	var externalID *string
	if id, ok := cfg[configAWSSNSRoleExternalID].(string); ok && id != "" {
		externalID = &id
	}

	// Without an explicit target role, use the subscriber's local AWS credentials.
	// Intermediary and target role assumption are only used for an explicit role override.
	intermediaryRoles := p.intermediaryRoles
	if roleARN == "" {
		intermediaryRoles = nil
		externalID = nil
	}

	snsClient, err := newSNSClientFn(ctx, parsed.Region, roleARN, externalID, intermediaryRoles)
	if err != nil {
		return nil, "", "", err
	}
	return snsClient, topicARN, parsed.Region, nil
}