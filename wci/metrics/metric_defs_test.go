package metrics

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
)

type recordingHandler struct {
	client.MetricsHandler
	tags map[string]string
}

func (h *recordingHandler) WithTags(tags map[string]string) client.MetricsHandler {
	merged := map[string]string{}
	for k, v := range h.tags {
		merged[k] = v
	}
	for k, v := range tags {
		merged[k] = v
	}
	return &recordingHandler{MetricsHandler: h.MetricsHandler, tags: merged}
}

func TestSessionTagValue(t *testing.T) {
	require.Equal(t, "true", SessionTagValue(true))
	require.Equal(t, "false", SessionTagValue(false))
}

func TestWithSessionTag(t *testing.T) {
	tests := []struct {
		name      string
		isSession bool
		want      string
	}{
		{name: "session", isSession: true, want: "true"},
		{name: "version", isSession: false, want: "false"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := &recordingHandler{MetricsHandler: client.MetricsNopHandler, tags: map[string]string{NamespaceTag: "ns"}}
			h := WithSessionTag(base, tc.isSession).(*recordingHandler)
			require.Equal(t, map[string]string{NamespaceTag: "ns", SessionTagName: tc.want}, h.tags)
		})
	}
}
