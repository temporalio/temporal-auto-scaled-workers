package iface

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.temporal.io/server/common/sdk"
)

func validSessionArgs() *WorkerControllerInstanceSessionArgs {
	return &WorkerControllerInstanceSessionArgs{
		SessionID:    "session-0123456789abcdef0123456789abcdef",
		SessionQueue: "session-queue",
	}
}

func TestSessionArgs_Validate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(s *WorkerControllerInstanceSessionArgs) *WorkerControllerInstanceSessionArgs
		wantErr []string
	}{
		{
			name:   "valid",
			mutate: func(s *WorkerControllerInstanceSessionArgs) *WorkerControllerInstanceSessionArgs { return s },
		},
		{
			name: "valid with colons",
			mutate: func(s *WorkerControllerInstanceSessionArgs) *WorkerControllerInstanceSessionArgs {
				s.SessionID, s.SessionQueue = "a:b", "q:1"
				return s
			},
		},
		{
			name:    "nil",
			mutate:  func(*WorkerControllerInstanceSessionArgs) *WorkerControllerInstanceSessionArgs { return nil },
			wantErr: []string{"session args are required"},
		},
		{
			name: "missing session ID",
			mutate: func(s *WorkerControllerInstanceSessionArgs) *WorkerControllerInstanceSessionArgs {
				s.SessionID = ""
				return s
			},
			wantErr: []string{"session ID is required"},
		},
		{
			name: "missing session queue",
			mutate: func(s *WorkerControllerInstanceSessionArgs) *WorkerControllerInstanceSessionArgs {
				s.SessionQueue = ""
				return s
			},
			wantErr: []string{"session queue is required"},
		},
		{
			name: "all empty reports every field",
			mutate: func(*WorkerControllerInstanceSessionArgs) *WorkerControllerInstanceSessionArgs {
				return &WorkerControllerInstanceSessionArgs{}
			},
			wantErr: []string{"session ID is required", "session queue is required"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.mutate(validSessionArgs()).Validate()
			if len(tc.wantErr) == 0 {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, msg := range tc.wantErr {
				require.ErrorContains(t, err, msg)
			}
		})
	}
}

func TestWorkflowArgs_IsSession(t *testing.T) {
	var nilArgs *WorkerControllerInstanceWorkflowArgs
	require.False(t, nilArgs.IsSession())
	require.False(t, (&WorkerControllerInstanceWorkflowArgs{}).IsSession())
	require.True(t, (&WorkerControllerInstanceWorkflowArgs{Session: validSessionArgs()}).IsSession())
}

func TestWorkflowArgs_SessionRoundTrip(t *testing.T) {
	next := timestamppb.New(time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC))
	in := &WorkerControllerInstanceWorkflowArgs{
		NamespaceName:  "ns",
		NamespaceId:    "ns-id",
		DeploymentName: "my-deployment",
		BuildId:        "build-1",
		State: &WorkerControllerInstanceLocalState{
			ScalingStatus:        map[string]ScalingAlgorithmStatus{},
			LastNoSyncSignalTime: next,
		},
		Session: validSessionArgs(),
	}

	payload, err := sdk.PreferProtoDataConverter.ToPayload(in)
	require.NoError(t, err)

	var out WorkerControllerInstanceWorkflowArgs
	require.NoError(t, sdk.PreferProtoDataConverter.FromPayload(payload, &out))
	require.Equal(t, in.Session, out.Session)
	require.True(t, out.IsSession())
	require.NotNil(t, out.State)
	require.True(t, next.AsTime().Equal(out.State.LastNoSyncSignalTime.AsTime()))
}

func TestWorkflowArgs_VersionArgsOmitSessionFields(t *testing.T) {
	in := &WorkerControllerInstanceWorkflowArgs{
		DeploymentName: "my-deployment",
		BuildId:        "build-1",
		State:          &WorkerControllerInstanceLocalState{},
	}
	payload, err := sdk.PreferProtoDataConverter.ToPayload(in)
	require.NoError(t, err)
	require.NotContains(t, string(payload.GetData()), `"session"`)
	require.NotContains(t, string(payload.GetData()), "last_no_sync_signal_time")
}

// Args and state serialized before session mode existed must still decode as a version WCI.
func TestWorkflowArgs_LegacyPayloadDecodes(t *testing.T) {
	legacy := &WorkerControllerInstanceWorkflowArgs{
		NamespaceName:  "ns",
		DeploymentName: "my-deployment",
		BuildId:        "build-1",
		State: &WorkerControllerInstanceLocalState{
			ScalingStatus: map[string]ScalingAlgorithmStatus{},
			NextPollTime:  timestamppb.New(time.Unix(100, 0)),
		},
	}
	payload, err := sdk.PreferProtoDataConverter.ToPayload(legacy)
	require.NoError(t, err)
	// Same encoding, but from raw JSON written by an older binary (no session keys).
	payload.Data = []byte(`{"namespace_name":"ns","deployment_name":"my-deployment","build_id":"build-1","state":{"scaling_state":{},"next_poll_time":{"seconds":100}}}`)

	var out WorkerControllerInstanceWorkflowArgs
	require.NoError(t, sdk.PreferProtoDataConverter.FromPayload(payload, &out))
	require.False(t, out.IsSession())
	require.Nil(t, out.Session)
	require.NotNil(t, out.State)
	require.Nil(t, out.State.LastNoSyncSignalTime)
	require.Equal(t, int64(100), out.State.NextPollTime.AsTime().Unix())
}
