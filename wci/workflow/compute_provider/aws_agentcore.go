package computeprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	agentcore_dp "github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	agentcore_cp "github.com/aws/aws-sdk-go-v2/service/bedrockagentcorecontrol"

	"go.temporal.io/auto-scaled-workers/wci/client"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
)

const (
	configAWSAgentCoreEndpointARN    = "endpoint_arn"
	configAWSAgentCoreRole           = "role"
	configAWSAgentCoreRoleExternalID = "role_external_id"
)

type AgentCoreParams struct {
	Role       string
	ExternalID *string

	// The following are derived from Runtime Endpoint ARN input
	RuntimeID    string
	EndpointName string
	AccountID    string
	Region       string
}

type awsAgentCoreComputeProvider struct {
	intermediaryRoles        [][]client.AWSIAMRoleRequest
	requireRoleAndExternalID bool
}

type agentCoreDataAPI interface {
	InvokeAgentRuntime(ctx context.Context, params *agentcore_dp.InvokeAgentRuntimeInput, optFns ...func(*agentcore_dp.Options)) (*agentcore_dp.InvokeAgentRuntimeOutput, error)
}

type agentCoreControlAPI interface {
	GetAgentRuntimeEndpoint(ctx context.Context, params *agentcore_cp.GetAgentRuntimeEndpointInput, optFns ...func(*agentcore_cp.Options)) (*agentcore_cp.GetAgentRuntimeEndpointOutput, error)
}

func init() {
	RegisterComputeProvider(iface.ComputeProviderTypeAWSAgentCore, NewAWSAgentCoreComputeProvider)
}

func NewAWSAgentCoreComputeProvider(_ context.Context, dc *dynamicconfig.Collection) (ComputeProvider, error) {
	var intermediaryRoles [][]client.AWSIAMRoleRequest
	requireRoleAndExternalID := true
	if dc != nil {
		intermediaryRoles = client.WorkerControllerAWSIntermediaryRoles.Get(dc)()
		requireRoleAndExternalID = client.WorkerControllerAWSRequireRoleAndExternalID.Get(dc)()
	}

	return &awsAgentCoreComputeProvider{
		intermediaryRoles:        intermediaryRoles,
		requireRoleAndExternalID: requireRoleAndExternalID,
	}, nil
}

func (p *awsAgentCoreComputeProvider) LaunchStrategy() LaunchStrategy {
	return LaunchStrategyInvoke
}

func (p *awsAgentCoreComputeProvider) ValidateConfig(ctx context.Context, _ RequestContext, cfg ComputeProviderConfig) error {
	if p.requireRoleAndExternalID {
		if roleARN, _ := cfg[configAWSAgentCoreRole].(string); roleARN == "" {
			return fmt.Errorf("AWS AgentCore compute provider requires %q to be configured", configAWSAgentCoreRole)
		}
		if eid, _ := cfg[configAWSAgentCoreRoleExternalID].(string); eid == "" {
			return fmt.Errorf("AWS AgentCore compute provider requires %q to be configured", configAWSAgentCoreRoleExternalID)
		}
	}

	endpointARN, _ := cfg[configAWSAgentCoreEndpointARN].(string)
	if endpointARN == "" {
		return fmt.Errorf("AWS AgentCore compute provider requires %q to be configured", configAWSAgentCoreEndpointARN)
	}

	controlClient, params, err := p.getControlClientAndParams(ctx, cfg)
	if err != nil {
		return fmt.Errorf("cannot connect to the compute provider: %w", err)
	}

	// Validate the specific runtime endpoint (runtime id + endpoint name), both
	// parsed out of the endpoint ARN.
	if _, err = controlClient.GetAgentRuntimeEndpoint(ctx, &agentcore_cp.GetAgentRuntimeEndpointInput{
		AgentRuntimeId: aws.String(params.RuntimeID),
		EndpointName:   aws.String(params.EndpointName),
	}); err != nil {
		return fmt.Errorf("cannot access the compute resource: %w", err)
	}

	if err := p.checkExternalID(ctx, cfg, params.Region); err != nil {
		return fmt.Errorf("IAM role trust policy does not enforce ExternalID condition: %w", err)
	}

	return nil
}

func (p *awsAgentCoreComputeProvider) InvokeWorker(ctx context.Context, rc RequestContext, cfg ComputeProviderConfig) error {
	err := p.invokeWorker(ctx, rc, cfg)
	return NewProviderError(classifyAWSFailure(err), err)
}

func (p *awsAgentCoreComputeProvider) invokeWorker(ctx context.Context, rc RequestContext, cfg ComputeProviderConfig) error {
	invokeClient, params, err := p.getDataClientAndParams(ctx, cfg)
	if err != nil {
		return err
	}

	// Provide worker deployment version details in the payload
	payload, err := json.Marshal(struct {
		DeploymentName string `json:"deploymentName"`
		BuildID        string `json:"buildId"`
	}{
		DeploymentName: rc.DeploymentName,
		BuildID:        rc.DeploymentBuildID,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal AgentCore invoke payload: %w", err)
	}

	// Account ID is required with runtime ID and endpoint name is passed as qualifier to invoke specific version
	input := &agentcore_dp.InvokeAgentRuntimeInput{
		AgentRuntimeArn: aws.String(params.RuntimeID),
		AccountId:       aws.String(params.AccountID),
		ContentType:     aws.String("application/json"),
		Payload:         payload,
		Qualifier:       aws.String(params.EndpointName),
	}

	// AgentCore doesn't have an async invocation method. This aims to make the call and close the response stream on
	// our end as soon as possible, preventing this activity from blocking.
	resp, err := invokeClient.InvokeAgentRuntime(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to invoke AgentCore runtime: %w", err)
	}
	if resp.Response != nil {
		_ = resp.Response.Close()
	}
	if resp.StatusCode != nil && (*resp.StatusCode < 200 || *resp.StatusCode >= 300) {
		return fmt.Errorf("failed to invoke AgentCore runtime: status code %d", *resp.StatusCode)
	}

	return nil
}

func (p *awsAgentCoreComputeProvider) UpdateWorkerSetSize(_ context.Context, _ RequestContext, _ ComputeProviderConfig, _ int32) error {
	return errors.ErrUnsupported
}

func (p *awsAgentCoreComputeProvider) checkExternalID(ctx context.Context, cfg ComputeProviderConfig, region string) error {
	roleARN, _ := cfg[configAWSAgentCoreRole].(string)
	eid, _ := cfg[configAWSAgentCoreRoleExternalID].(string)
	if roleARN == "" || eid == "" {
		return nil
	}
	if region == "" {
		return fmt.Errorf("cannot verify external ID enforcement for role %q: region is required", roleARN)
	}
	return verifyExternalIDEnforcedFn(ctx, region, roleARN, p.intermediaryRoles)
}

// Parses Runtime Endpoint ARN into pieces required by various APIs
// The Get and Invoke APIs are not symmetric with inputs, luckily the Endpoint ARN is a subresource or Runtime ARN and
// contains all we need.
func parseAgentCoreEndpointARN(endpointARN string) (runtimeID, endpointName, accountID, region string, err error) {
	parsed, err := arn.Parse(endpointARN)
	if err != nil {
		return "", "", "", "", fmt.Errorf("failed to parse AgentCore endpoint ARN %q: %w", endpointARN, err)
	}
	if parsed.Region == "" {
		return "", "", "", "", fmt.Errorf("AgentCore endpoint ARN %q does not contain a region", endpointARN)
	}
	if parsed.AccountID == "" {
		return "", "", "", "", fmt.Errorf("AgentCore endpoint ARN %q does not contain an account id", endpointARN)
	}
	// resource is runtime/RUNTIME_ID/runtime-endpoint/ENDPOINT_NAME
	parts := strings.Split(parsed.Resource, "/")
	if len(parts) != 4 || parts[0] != "runtime" || parts[2] != "runtime-endpoint" || parts[1] == "" || parts[3] == "" {
		return "", "", "", "", fmt.Errorf("AgentCore Endpoint ARN %q does not have correct pattern", endpointARN)
	}
	runtimeID = parts[1]
	endpointName = parts[3]
	return runtimeID, endpointName, parsed.AccountID, parsed.Region, nil
}

// newAgentCoreDataClientFn and newAgentCoreControlClientFn are package-level
// variables so tests can swap them for mocks without reaching AWS.
var (
	newAgentCoreDataClientFn    = newAgentCoreDataClient
	newAgentCoreControlClientFn = newAgentCoreControlClient
)

func newAgentCoreDataClient(ctx context.Context, region, roleARN string, externalID *string, intermediaryRoles [][]client.AWSIAMRoleRequest) (agentCoreDataAPI, error) {
	awsConfig, err := buildAWSConfig(ctx, region, roleARN, externalID, intermediaryRoles)
	if err != nil {
		return nil, err
	}
	return agentcore_dp.NewFromConfig(awsConfig), nil
}

func newAgentCoreControlClient(ctx context.Context, region, roleARN string, externalID *string, intermediaryRoles [][]client.AWSIAMRoleRequest) (agentCoreControlAPI, error) {
	awsConfig, err := buildAWSConfig(ctx, region, roleARN, externalID, intermediaryRoles)
	if err != nil {
		return nil, err
	}
	return agentcore_cp.NewFromConfig(awsConfig), nil
}

// resolveParams ensures the required inputs are present and derives the runtime
// ARN, runtime id, endpoint name, and region from the single endpoint ARN.
func (p *awsAgentCoreComputeProvider) resolveParams(cfg ComputeProviderConfig) (AgentCoreParams, error) {
	endpointARN, ok := cfg[configAWSAgentCoreEndpointARN].(string)
	if !ok || endpointARN == "" {
		return AgentCoreParams{}, fmt.Errorf("AWS AgentCore endpoint ARN not found or invalid")
	}

	runtimeID, endpointName, accountID, region, err := parseAgentCoreEndpointARN(endpointARN)
	if err != nil {
		return AgentCoreParams{}, err
	}

	roleARN, _ := cfg[configAWSAgentCoreRole].(string)
	if roleARN != "" {
		if err := validateRoleARN(roleARN); err != nil {
			return AgentCoreParams{}, err
		}
	}

	var roleExternalID *string
	if eid, ok := cfg[configAWSAgentCoreRoleExternalID].(string); ok && eid != "" {
		roleExternalID = &eid
	}

	return AgentCoreParams{
		RuntimeID:    runtimeID,
		EndpointName: endpointName,
		AccountID:    accountID,
		Role:         roleARN,
		ExternalID:   roleExternalID,
		Region:       region,
	}, nil
}

// Gets the dp client and parsed parameters with some validation
func (p *awsAgentCoreComputeProvider) getDataClientAndParams(ctx context.Context, cfg ComputeProviderConfig) (agentCoreDataAPI, AgentCoreParams, error) {
	params, err := p.resolveParams(cfg)
	if err != nil {
		return nil, params, err
	}
	c, err := newAgentCoreDataClientFn(ctx, params.Region, params.Role, params.ExternalID, p.intermediaryRoles)
	if err != nil {
		return nil, AgentCoreParams{}, err
	}
	return c, params, nil
}

// Gets the cp client and parsed parameters with some validation
func (p *awsAgentCoreComputeProvider) getControlClientAndParams(ctx context.Context, cfg ComputeProviderConfig) (agentCoreControlAPI, AgentCoreParams, error) {
	params, err := p.resolveParams(cfg)
	if err != nil {
		return nil, params, err
	}
	c, err := newAgentCoreControlClientFn(ctx, params.Region, params.Role, params.ExternalID, p.intermediaryRoles)
	if err != nil {
		return nil, AgentCoreParams{}, err
	}
	return c, params, nil
}
