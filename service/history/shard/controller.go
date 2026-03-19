package shard

import (
	"context"

	"github.com/hanzoai/tasks/common/namespace"
	"github.com/hanzoai/tasks/common/pingable"
	historyi "github.com/hanzoai/tasks/service/history/interfaces"
)

//go:generate mockgen -package $GOPACKAGE -source $GOFILE -destination controller_mock.go

type (
	Controller interface {
		pingable.Pingable

		GetShardByID(shardID int32) (historyi.ShardContext, error)
		GetShardByNamespaceWorkflow(namespaceID namespace.ID, workflowID string) (historyi.ShardContext, error)
		CloseShardByID(shardID int32)
		ShardIDs() []int32
		Start()
		Stop()
		// InitialShardsAcquired blocks until initial shard acquisition is complete, context timeout,
		// or Stop is called. Returns nil if shards are acquired, otherwise context error (on Stop,
		// returns context.Canceled).
		InitialShardsAcquired(context.Context) error
	}
)
