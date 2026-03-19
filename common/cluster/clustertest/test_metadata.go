package clustertest

import (
	"github.com/hanzoai/tasks/common/cluster"
	"github.com/hanzoai/tasks/common/log"
)

// NewMetadataForTest returns a new [cluster.Metadata] instance for testing.
func NewMetadataForTest(
	config *cluster.Config,
) cluster.Metadata {
	return cluster.NewMetadata(
		config.EnableGlobalNamespace,
		config.FailoverVersionIncrement,
		config.MasterClusterName,
		config.CurrentClusterName,
		config.ClusterInformation,
		nil,
		nil,
		log.NewNoopLogger(),
	)
}
