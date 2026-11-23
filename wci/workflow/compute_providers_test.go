package workflow

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.temporal.io/auto-scaled-workers/wci/client"
	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
	"go.temporal.io/auto-scaled-workers/wci/workflow/iface"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
)

func TestComputeProviders_ModalRegistered(t *testing.T) {
	dc := dynamicconfig.NewCollection(dynamicconfig.StaticClient{
		client.WorkerControllerEnabledComputeProviders.Key(): []string{string(iface.ComputeProviderTypeModalFunction)},
	}, log.NewNoopLogger())

	provider, err := computeprovider.GetComputeProvider(t.Context(), iface.ComputeProviderTypeModalFunction, "ns.acct", dc)
	require.NoError(t, err)
	require.NotNil(t, provider, "the modal package must be imported here for the provider to register")
}
