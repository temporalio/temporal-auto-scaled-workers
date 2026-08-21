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
	configAWSAgentCoreRuntimeARN      = "runtime_arn"
	configAWSAgentCoreRole            = "role"
	configAWSAgentCoreRoleExternalID  = "role_external_id"
	configAWSAgentCoreRuntimeEndpoint = "runtime_endpoint"
)

type AgentCoreParams struct {
	RuntimeARN      string
	RuntimeEndpoint string
	Role            string
	ExternalID      *string
	Region          string
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

	endpoint, _ := cfg[configAWSAgentCoreRuntimeEndpoint].(string)
	if endpoint == "" {
		return fmt.Errorf("AWS AgentCore compute provider requires %q to be configured", configAWSAgentCoreRuntimeEndpoint)
	}

	controlClient, params, err := p.getControlClientAndParams(ctx, cfg)
	if err != nil {
		return fmt.Errorf("cannot connect to the compute provider: %w", err)
	}

	runtimeID, err := extractAgentCoreRuntimeID(params.RuntimeARN)
	if err != nil {
		return err
	}

	// Validate the specific runtime endpoint (runtime + endpoint name).
	if _, err = controlClient.GetAgentRuntimeEndpoint(ctx, &agentcore_cp.GetAgentRuntimeEndpointInput{
		AgentRuntimeId: aws.String(runtimeID),
		EndpointName:   aws.String(params.RuntimeEndpoint),
	}); err != nil {
		return fmt.Errorf("cannot access the compute resource: %w", err)
	}

	if err := p.checkExternalID(ctx, cfg, params.RuntimeARN); err != nil {
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

	// The endpoint name is passed through as the AgentCore invoke Qualifier.
	input := &agentcore_dp.InvokeAgentRuntimeInput{
		AgentRuntimeArn: aws.String(params.RuntimeARN),
		ContentType:     aws.String("application/json"),
		Payload:         payload,
		Qualifier:       aws.String(params.RuntimeEndpoint),
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

func (p *awsAgentCoreComputeProvider) checkExternalID(ctx context.Context, cfg ComputeProviderConfig, runtimeARN string) error {
	roleARN, _ := cfg[configAWSAgentCoreRole].(string)
	eid, _ := cfg[configAWSAgentCoreRoleExternalID].(string)
	if roleARN == "" || eid == "" {
		return nil
	}
	region, err := extractRegionFromARN(runtimeARN)
	if err != nil {
		return fmt.Errorf("cannot verify external ID enforcement: failed to extract region from AgentCore runtime ARN %q: %w", runtimeARN, err)
	}
	return verifyExternalIDEnforcedFn(ctx, region, roleARN, p.intermediaryRoles)
}

// extractAgentCoreRuntimeID pulls the runtime id out of a runtime ARN. AgentCore
// runtime ARNs look like arn:aws:bedrock-agentcore:REGION:ACCOUNT:runtime/ID, and
// the controlplane GetAgentRuntimeEndpoint API keys on the id rather than the ARN.
func extractAgentCoreRuntimeID(runtimeARN string) (string, error) {
	parsed, err := arn.Parse(runtimeARN)
	if err != nil {
		return "", fmt.Errorf("failed to parse AgentCore runtime ARN %q: %w", runtimeARN, err)
	}
	id := strings.TrimPrefix(parsed.Resource, "runtime/")
	if id == "" || id == parsed.Resource {
		return "", fmt.Errorf("AgentCore runtime ARN %q does not contain a runtime id", runtimeARN)
	}
	return id, nil
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

// Ensures required inputs are present and region is in runtime ARN, returns parsed values
func (p *awsAgentCoreComputeProvider) resolveParams(cfg ComputeProviderConfig) (AgentCoreParams, error) {
	runtimeARN, ok := cfg[configAWSAgentCoreRuntimeARN].(string)
	if !ok || runtimeARN == "" {
		return AgentCoreParams{}, fmt.Errorf("AWS AgentCore Runtime ARN not found or invalid")
	}
	endpoint, ok := cfg[configAWSAgentCoreRuntimeEndpoint].(string)
	if !ok || endpoint == "" {
		return AgentCoreParams{}, fmt.Errorf("AWS AgentCore Runtime Endpoint not found or invalid")
	}

	region, err := extractRegionFromARN(runtimeARN)
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
		RuntimeARN:      runtimeARN,
		RuntimeEndpoint: endpoint,
		Role:            roleARN,
		ExternalID:      roleExternalID,
		Region:          region,
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
