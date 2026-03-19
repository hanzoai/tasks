package api

import (
	"go.temporal.io/api/serviceerror"
	"github.com/hanzoai/tasks/common/cluster"
)

func ValidateReplicationConfig(
	clusterMetadata cluster.Metadata,
) error {
	if !clusterMetadata.IsGlobalNamespaceEnabled() {
		return serviceerror.NewUnavailable("The cluster has global namespace disabled. The operation is not supported.")
	}
	return nil
}
