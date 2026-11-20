package modal

import (
	"fmt"
	"strconv"
	"time"

	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/server/common/dynamicconfig"
)

const (
	configAppName      = "app_name"
	configFunctionName = "function_name"
	configVersion      = "version"
	configEnvironment  = "environment"
	configWorkspaceID  = "workspace_id"
)

// Params is a worker deployment version's resolved Modal configuration.
type Params struct {
	// AppName is the deployed Modal App.
	AppName string
	// FunctionName is the Function in the App that runs a worker.
	FunctionName string
	// Environment is the Modal environment the App is deployed in.
	Environment string
	// WorkspaceID selects which credentials the SecretSource returns.
	WorkspaceID string
	// Version pins a deployed App version; zero means the latest deployment.
	Version int
}

// positiveSetting reads s, falling back to def when the value is 0 or less.
func positiveSetting(dc *dynamicconfig.Collection, s dynamicconfig.GlobalIntSetting, def int) int {
	if v := s.Get(dc)(); v > 0 {
		return v
	}
	return def
}

func positiveSeconds(dc *dynamicconfig.Collection, s dynamicconfig.GlobalIntSetting, def time.Duration) time.Duration {
	if v := s.Get(dc)(); v > 0 {
		return time.Duration(v) * time.Second
	}
	return def
}

func requireWorkspace(params Params) error {
	if params.WorkspaceID != "" {
		return nil
	}
	return fmt.Errorf("the Modal compute provider requires %q on this server: set it (--workspace-id in "+
		"the Temporal CLI) to the Modal workspace ID, which starts with \"ac-\"", configWorkspaceID)
}

func resolveParams(cfg computeprovider.ComputeProviderConfig) (Params, error) {
	appName, _ := cfg[configAppName].(string)
	if appName == "" {
		return Params{}, fmt.Errorf("the Modal compute provider requires %q to be configured", configAppName)
	}
	functionName, _ := cfg[configFunctionName].(string)
	if functionName == "" {
		return Params{}, fmt.Errorf("the Modal compute provider requires %q to be configured", configFunctionName)
	}
	environment, _ := cfg[configEnvironment].(string)
	if environment == "" {
		return Params{}, fmt.Errorf("the Modal compute provider requires %q to be configured", configEnvironment)
	}
	// workspace_id is optional: the SDK default does not need it, and requireWorkspace enforces it otherwise.
	workspaceID, _ := cfg[configWorkspaceID].(string)

	version, err := parseConfigVersion(cfg)
	if err != nil {
		return Params{}, err
	}

	return Params{
		AppName:      appName,
		FunctionName: functionName,
		Environment:  environment,
		WorkspaceID:  workspaceID,
		Version:      version,
	}, nil
}

// parseConfigVersion parses a decimal string, as all compute provider config is string-valued.
func parseConfigVersion(cfg computeprovider.ComputeProviderConfig) (int, error) {
	raw, ok := cfg[configVersion]
	if !ok {
		return 0, nil
	}

	s, ok := raw.(string)
	if ok && s == "" {
		return 0, nil
	}
	version, err := strconv.Atoi(s)
	if !ok || err != nil || version < 0 {
		return 0, fmt.Errorf("the Modal compute provider %q must be a non-negative integer string", configVersion)
	}
	return version, nil
}
