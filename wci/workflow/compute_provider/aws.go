package computeprovider

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithy "github.com/aws/smithy-go"
	"go.temporal.io/auto-scaled-workers/wci/client"
)

type stsAPI interface {
	AssumeRole(ctx context.Context, params *sts.AssumeRoleInput, optFns ...func(*sts.Options)) (*sts.AssumeRoleOutput, error)
}

// errWCIOwned marks a failure as attributable to worker-controller's own AWS setup
// (base credentials, intermediary role chain) rather than the customer's config.
// This has to be marked where it happens: intermediary and customer role
// assumptions go through the same helper and fail indistinguishably.
var errWCIOwned = errors.New("worker-controller AWS configuration error")

var (
	awsThrottleCheck   = retry.ThrottleErrorCode{Codes: retry.DefaultThrottleErrorCodes}
	awsConnectionCheck = retry.RetryableConnectionError{}
)

// classifyAWSFailure maps an AWS SDK error onto a FailureClass along two axes:
// what kind of failure it was, read from the smithy error model, and whose config
// caused it, read from errWCIOwned.
func classifyAWSFailure(err error) FailureClass {
	if err == nil {
		return FailureUnclassified
	}

	// A cancelled request tells us nothing about either axis; don't blame it on
	// whoever's config happened to be in play.
	if errors.Is(err, context.Canceled) {
		return FailureUnclassified
	}

	// Ownership separates client-fault from WCI-fault errors. A throttled or
	// server-side failure is the provider's regardless of whose config we used.
	wciOwned := errors.Is(err, errWCIOwned)
	ownerFault := FailureRejected
	if wciOwned {
		ownerFault = FailureInternal
	}

	// Throttles are modelled as client faults, so they must be checked before the
	// fault split or every one of them lands in the rejected bucket.
	if awsThrottleCheck.IsErrorThrottle(err) == aws.TrueTernary {
		return FailureThrottled
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		if apiErr.ErrorFault() == smithy.FaultServer {
			return FailureUnavailable
		}
		// Our own missing resources and denied permissions page the same on-call in
		// the same way, so there is nothing to gain from narrowing them.
		if wciOwned {
			return FailureInternal
		}
		return classifyAWSRejection(apiErr.ErrorCode())
	}

	// No modelled API error: the request either failed in transport or never left
	// the process. Local validation failures fall through to ownerFault.
	if awsConnectionCheck.IsErrorRetryable(err) == aws.TrueTernary || errors.Is(err, context.DeadlineExceeded) {
		return FailureUnavailable
	}
	return ownerFault
}

// classifyAWSRejection narrows a customer-caused client fault by error code. AWS
// names these consistently across services -- a missing resource carries NotFound,
// a permissions failure carries AccessDenied -- so matching the convention covers
// services we have not integrated yet, where an explicit code list would not.
// Anything unrecognized stays in the unnarrowed bucket rather than being guessed.
func classifyAWSRejection(code string) FailureClass {
	switch {
	case strings.Contains(code, "NotFound"):
		return FailureNotFound
	case strings.Contains(code, "AccessDenied"):
		return FailureAccessDenied
	default:
		return FailureRejected
	}
}

// buildBaseAWSConfig loads default AWS config and applies any intermediary roles.
// Every input here comes from worker-controller's own configuration, so all of its
// failures are marked errWCIOwned.
func buildBaseAWSConfig(ctx context.Context, region string, intermediaryRoles [][]client.AWSIAMRoleRequest) (aws.Config, error) {
	awsConfig, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return aws.Config{}, fmt.Errorf("%w: failed to load AWS config: %w", errWCIOwned, err)
	}

	// the AWS compute providers support role chaining to enable abstracting the role the temporal server has
	// from the role that can assume the final execution/invocation role. This might be used in more complex
	// setups to allow for rotating the AWS accounts used for both ends. Basically emulating service principals.
	for _, step := range intermediaryRoles {
		if len(step) == 0 {
			continue
		}

		// At each stage of the role chaining, one of multiple roles might be chosen to proceed. This allows for load balancing
		// requests across roles/accounts and reduces the impact of any of these accounts failing (until it is removed from the config).
		req := step[rand.Intn(len(step))]
		if err = validateRoleARN(req.RoleARN); err != nil {
			return aws.Config{}, fmt.Errorf("%w: invalid intermediary role: %w", errWCIOwned, err)
		}
		if req.RoleSessionName == "" {
			return aws.Config{}, fmt.Errorf("%w: empty role session name for intermediary role", errWCIOwned)
		}

		awsConfig, err = assumeRoleWithRequest(ctx, awsConfig, &req)
		if err != nil {
			return aws.Config{}, fmt.Errorf("%w: error assuming intermediary role: %w", errWCIOwned, err)
		}
	}
	return awsConfig, nil
}

// buildAWSConfig loads the default AWS config for the given region, applies any
// intermediary role chain, then optionally assumes a final role.
func buildAWSConfig(ctx context.Context, region, roleARN string, externalID *string, intermediaryRoles [][]client.AWSIAMRoleRequest) (aws.Config, error) {
	awsConfig, err := buildBaseAWSConfig(ctx, region, intermediaryRoles)
	if err != nil {
		return aws.Config{}, err
	}
	if roleARN != "" {
		awsConfig, err = assumeRoleWithRequest(ctx, awsConfig, &client.AWSIAMRoleRequest{
			RoleARN:         roleARN,
			RoleSessionName: "managed-workers-aws",
			ExternalID:      externalID,
		})
		if err != nil {
			return aws.Config{}, err
		}
	}
	return awsConfig, nil
}

// checkExternalIDEnforced is the testable core: assumes roleARN with no external ID.
// If the call succeeds the trust policy does not enforce the external ID → error.
// An AccessDenied response confirms enforcement. Any other error (network, throttle, etc.)
// is surfaced so the caller knows the check could not be completed.
func checkExternalIDEnforced(ctx context.Context, stsClient stsAPI, roleARN string) error {
	_, err := stsClient.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String(roleARN),
		RoleSessionName: aws.String("managed-workers-aws"),
	})
	if err == nil {
		return fmt.Errorf("role %s does not require the configured external ID; update the role's trust policy to enforce the aws:ExternalId condition", roleARN)
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "AccessDenied" {
		return nil
	}
	return fmt.Errorf("unable to verify external ID enforcement for role %s: %w", roleARN, err)
}

// verifyExternalIDEnforcedFn is a package-level variable so tests can swap it out without
// network calls.
var verifyExternalIDEnforcedFn = verifyExternalIDEnforced

// verifyExternalIDEnforced builds the base config then delegates to checkExternalIDEnforced.
func verifyExternalIDEnforced(ctx context.Context, region, roleARN string, intermediaryRoles [][]client.AWSIAMRoleRequest) error {
	baseConfig, err := buildBaseAWSConfig(ctx, region, intermediaryRoles)
	if err != nil {
		return err
	}
	return checkExternalIDEnforced(ctx, sts.NewFromConfig(baseConfig), roleARN)
}

func assumeRoleWithRequest(ctx context.Context, cfg aws.Config, req *client.AWSIAMRoleRequest) (aws.Config, error) {
	stsClient := sts.NewFromConfig(cfg)

	input := &sts.AssumeRoleInput{
		RoleArn:         aws.String(req.RoleARN),
		RoleSessionName: aws.String(req.RoleSessionName),
	}
	if req.ExternalID != nil && *req.ExternalID != "" {
		input.ExternalId = req.ExternalID
	}

	result, err := stsClient.AssumeRole(ctx, input)
	if err != nil {
		return cfg, fmt.Errorf("failed to assume role %s: %w", req.RoleARN, err)
	}

	if result.Credentials == nil ||
		result.Credentials.AccessKeyId == nil ||
		result.Credentials.SecretAccessKey == nil ||
		result.Credentials.SessionToken == nil {
		return cfg, fmt.Errorf("AssumeRole for %s returned incomplete credentials", req.RoleARN)
	}

	cfg.Credentials = credentials.NewStaticCredentialsProvider(
		*result.Credentials.AccessKeyId,
		*result.Credentials.SecretAccessKey,
		*result.Credentials.SessionToken,
	)

	return cfg, nil
}

func extractRegionFromARN(arnString string) (string, error) {
	parsedARN, err := arn.Parse(arnString)
	if err != nil {
		return "", err
	}
	if parsedARN.Region == "" {
		return "", fmt.Errorf("region not found in ARN: %s", arnString)
	}
	return parsedARN.Region, nil
}

func validateRoleARN(roleARN string) error {
	parsedARN, err := arn.Parse(roleARN)
	if err != nil {
		return err
	}
	if parsedARN.Service != "iam" {
		return fmt.Errorf("role ARN must be an IAM resource: %s", roleARN)
	}
	if !strings.HasPrefix(parsedARN.Resource, "role/") {
		return fmt.Errorf("role ARN must reference a role: %s", roleARN)
	}
	return nil
}
