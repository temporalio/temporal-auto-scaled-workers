package workflow

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	sdkworker "go.temporal.io/sdk/worker"
	sdkworkflow "go.temporal.io/sdk/workflow"
)

// TestReplayVersionWorkflowHistories replays version WCI histories recorded with the
// workflow code before session mode was added (logic version CheckWorkerControllerEnabledVersion,
// both signal-queue patches applied) against the current code. They cover the poll timer
// with PullStats, the periodic validation timer, an update followed by continue-as-new, a
// task-add signal and delete. A non-determinism error here means version WCIs would break.
//
// The histories were generated against a dev server with stub activities, a 2s
// maxPollInterval and a 1m validation interval.
// The reliablePollCadence change ID in them was renamed to timerSelector to match
// upstream (f231bce), so they reflect what origin/main records.
func TestReplayVersionWorkflowHistories(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "replay", "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, files)

	activities := NewActivities(nil, nil, nil, nil)
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			replayer := sdkworker.NewWorkflowReplayer()
			replayer.RegisterWorkflowWithOptions(func(ctx sdkworkflow.Context, args *iface.WorkerControllerInstanceWorkflowArgs) error {
				// Versions and the enabled flag are read via MutableSideEffect, so the recorded
				// values win; the getters only matter if a marker is missing.
				return Workflow(ctx,
					func() WorkerControllerInstanceWorkflowVersion { return SessionModeVersion },
					func() bool { return true },
					func() int { return 100 },
					func() time.Duration { return time.Minute },
					args, activities)
			}, sdkworkflow.RegisterOptions{Name: iface.WorkerControllerInstanceWorkflowType})
			require.NoError(t, replayer.ReplayWorkflowHistoryFromJSONFile(nil, file))
		})
	}
}
