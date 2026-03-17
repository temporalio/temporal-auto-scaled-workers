//go:build !release

package computeprovider

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/temporalio/temporal-managed-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
)

const (
	// subprocessTimeoutArg is the argument passed to the timeout(1) command (GNU coreutils).
	subprocessTimeoutArg = "1m"

	configSubprocessCommand = "command"
	configSubprocessArgs    = "args"
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

func (p *subprocessComputeProvider) ValidateConfig(ctx context.Context, config iface.ComputeProviderConfig) error {
	command, ok := config[configSubprocessCommand].(string)
	if !ok || strings.TrimSpace(command) == "" {
		return fmt.Errorf("command not found in config")
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

func (p *subprocessComputeProvider) InvokeWorker(ctx context.Context, config iface.ComputeProviderConfig) error {
	command, ok := config[configSubprocessCommand].(string)
	if !ok || strings.TrimSpace(command) == "" {
		return fmt.Errorf("command not found in config")
	}

	args := []string{}
	if argsVal, ok := config[configSubprocessArgs].(string); ok {
		args = splitOptionalArgs(argsVal)
	}

	// Use timeout(1) to limit the child to 1 minute; do not wait for completion.
	// Use context.Background() so the subprocess is not killed when the activity returns.
	timeoutArgs := append([]string{subprocessTimeoutArg, command}, args...)
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

func (p *subprocessComputeProvider) UpdateWorkerSetSize(_ context.Context, _ iface.ComputeProviderConfig, _ int32) error {
	return errors.ErrUnsupported
}

func splitOptionalArgs(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
