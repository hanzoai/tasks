package temporal_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	persistencespb "github.com/hanzoai/tasks/api/persistence/v1"
	"github.com/hanzoai/tasks/common/cluster"
	"github.com/hanzoai/tasks/common/config"
	"github.com/hanzoai/tasks/common/log"
	"github.com/hanzoai/tasks/common/persistence"
	"github.com/hanzoai/tasks/temporal"
	"go.uber.org/mock/gomock"
)

func TestNewClusterMetadataLoader(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	metadataManager := persistence.NewMockClusterMetadataManager(ctrl)
	logger := log.NewNoopLogger()
	loader := temporal.NewClusterMetadataLoader(metadataManager, logger)
	ctx := context.Background()
	svc := &config.Config{
		ClusterMetadata: &cluster.Config{
			CurrentClusterName: "current_cluster",
			ClusterInformation: map[string]cluster.ClusterInformation{
				"current_cluster": {
					RPCAddress:  "rpc_old",
					HTTPAddress: "http_old",
				},
				"remote_cluster1": {
					RPCAddress:  "rpc_old",
					HTTPAddress: "http_old",
				},
			},
		},
	}

	metadataManager.EXPECT().ListClusterMetadata(gomock.Any(), gomock.Any()).Return(&persistence.ListClusterMetadataResponse{
		ClusterMetadata: []*persistence.GetClusterMetadataResponse{
			{
				ClusterMetadata: &persistencespb.ClusterMetadata{
					ClusterName:    "current_cluster",
					ClusterAddress: "rpc_new",
					HttpAddress:    "http_new",
				},
			},
			{
				ClusterMetadata: &persistencespb.ClusterMetadata{
					ClusterName:    "remote_cluster1",
					ClusterAddress: "rpc_new",
					HttpAddress:    "http_new",
				},
			},
			{
				ClusterMetadata: &persistencespb.ClusterMetadata{
					ClusterName:    "remote_cluster2",
					ClusterAddress: "rpc_new",
					HttpAddress:    "http_new",
				},
			},
		},
	}, nil)

	err := loader.LoadAndMergeWithStaticConfig(ctx, svc)
	require.NoError(t, err)

	assert.Equal(t, map[string]cluster.ClusterInformation{
		"current_cluster": {
			RPCAddress:  "rpc_old",
			HTTPAddress: "http_new",
		},
		"remote_cluster1": {
			RPCAddress:  "rpc_new",
			HTTPAddress: "http_new",
		},
		"remote_cluster2": {
			RPCAddress:  "rpc_new",
			HTTPAddress: "http_new",
		},
	}, svc.ClusterMetadata.ClusterInformation)
}
