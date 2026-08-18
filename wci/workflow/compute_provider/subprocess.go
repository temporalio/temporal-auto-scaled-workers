//go:build !release

package computeprovider

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
)

const (
	// defaultSubprocessTimeoutArg is the default argument passed to timeout(1) (GNU coreutils).
	defaultSubprocessTimeoutArg = "1m"

	configSubprocessCommand = "command"
	configSubprocessArgs    = "args"
	configSubprocessTimeout = "timeout"
)

type subprocessComputeProvider struct{}

func init() {
	RegisterComputeProvider(iface.ComputeProviderTypeSubprocess, NewSubprocessComputeProvider)
}

func NewSubprocessComputeProvider(_ context.Context, _ *dynamicconfig.Collection) (ComputeProvider, error) {
	return &subprocessComputeProvider{}, nil
}

func (p *subprocessComputeProvider) LaunchStrategy() LaunchStrategy {
	return LaunchStrategyInvoke
}

func (p *subprocessComputeProvider) ValidateConfig(_ context.Context, _ RequestContext, config ComputeProviderConfig) error {
	command, ok := config[configSubprocessCommand].(string)
	if !ok || strings.TrimSpace(command) == "" {
		return fmt.Errorf("command not found in config")
	}
	if _, err := getSubprocessTimeoutArg(config); err != nil {
		return err
	}

	if _, err := exec.LookPath("timeout"); err != nil {
		return fmt.Errorf("required GNU coreutils 'timeout' binary not found: %w", err)
	}

	command = strings.TrimSpace(command)
	if _, err := exec.LookPath(command); err != nil {
		return fmt.Errorf("command %q not found locally: %w", command, err)
	}
	return nil
}

func (p *subprocessComputeProvider) InvokeWorker(_ context.Context, _ RequestContext, config ComputeProviderConfig) error {
	command, ok := config[configSubprocessCommand].(string)
	if !ok || strings.TrimSpace(command) == "" {
		return fmt.Errorf("command not found in config")
	}

	args := []string{}
	if argsVal, ok := config[configSubprocessArgs].(string); ok {
		args = splitOptionalArgs(argsVal)
	}
	timeoutArg, err := getSubprocessTimeoutArg(config)
	if err != nil {
		return err
	}

	// Use timeout(1) to limit the child; do not wait for completion.
	// Use context.Background() so the subprocess is not killed when the activity returns.
	timeoutArgs := append([]string{timeoutArg, command}, args...)
	cmd := exec.CommandContext(context.Background(), "timeout", timeoutArgs...)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start subprocess: %w", err)
	}

	go func() {
		if err := cmd.Wait(); err != nil {
			// using plain printf as the activity logger might no longer be valid
			// at this point, and this provider is meant for local-dev only anyhow
			fmt.Printf("subprocess worker '%s' exited with error: %v\n", command, err)
		}
	}()
	return nil
}

func (p *subprocessComputeProvider) UpdateWorkerSetSize(_ context.Context, _ RequestContext, _ ComputeProviderConfig, _ int32) error {
	return errors.ErrUnsupported
}

func getSubprocessTimeoutArg(config ComputeProviderConfig) (string, error) {
	value, ok := config[configSubprocessTimeout]
	if !ok {
		return defaultSubprocessTimeoutArg, nil
	}

	timeout, ok := value.(string)
	duration, err := time.ParseDuration(timeout)
	if !ok || err != nil || duration <= 0 {
		return "", fmt.Errorf("timeout must be a positive duration string")
	}
	return fmt.Sprintf("%.9fs", duration.Seconds()), nil
}

func splitOptionalArgs(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
