package replication

import (
	"context"
	"fmt"
	"math"
	"time"

	"go.temporal.io/api/serviceerror"
	enumsspb "github.com/hanzoai/tasks/api/enums/v1"
	persistencespb "github.com/hanzoai/tasks/api/persistence/v1"
	replicationspb "github.com/hanzoai/tasks/api/replication/v1"
	"github.com/hanzoai/tasks/common/log/tag"
	"github.com/hanzoai/tasks/common/persistence"
	historyi "github.com/hanzoai/tasks/service/history/interfaces"
	"github.com/hanzoai/tasks/service/history/replication"
	"github.com/hanzoai/tasks/service/history/shard"
	"github.com/hanzoai/tasks/service/history/tasks"
)

func GetTasks(
	ctx context.Context,
	shardContext historyi.ShardContext,
	replicationAckMgr replication.AckManager,
	pollingCluster string,
	ackMessageID int64,
	ackTimestamp time.Time,
	queryMessageID int64,
) (*replicationspb.ReplicationMessages, error) {
	allClusterInfo := shardContext.GetClusterMetadata().GetAllClusterInfo()
	clusterInfo, ok := allClusterInfo[pollingCluster]
	if !ok {
		return nil, serviceerror.NewInternal(
			fmt.Sprintf("missing cluster info for cluster: %v", pollingCluster),
		)
	}
	readerID := shard.ReplicationReaderIDFromClusterShardID(
		clusterInfo.InitialFailoverVersion,
		shardContext.GetShardID(),
	)

	if ackMessageID != persistence.EmptyQueueMessageID {
		if err := shardContext.UpdateReplicationQueueReaderState(
			readerID,
			&persistencespb.QueueReaderState{
				Scopes: []*persistencespb.QueueSliceScope{{
					Range: &persistencespb.QueueSliceRange{
						InclusiveMin: shard.ConvertToPersistenceTaskKey(
							tasks.NewImmediateKey(ackMessageID + 1),
						),
						ExclusiveMax: shard.ConvertToPersistenceTaskKey(
							tasks.NewImmediateKey(math.MaxInt64),
						),
					},
					Predicate: &persistencespb.Predicate{
						PredicateType: enumsspb.PREDICATE_TYPE_UNIVERSAL,
						Attributes:    &persistencespb.Predicate_UniversalPredicateAttributes{},
					},
				}},
			},
		); err != nil {
			shardContext.GetLogger().Error("error updating replication level for shard", tag.Error(err), tag.OperationFailed)
		}
		shardContext.UpdateRemoteClusterInfo(pollingCluster, ackMessageID, ackTimestamp)
	}

	replicationMessages, err := replicationAckMgr.GetTasks(
		ctx,
		pollingCluster,
		queryMessageID,
	)
	if err != nil {
		shardContext.GetLogger().Error("Failed to retrieve replication messages.", tag.Error(err))
		return nil, err
	}

	shardContext.GetLogger().Debug("Successfully fetched replication messages.", tag.Counter(len(replicationMessages.ReplicationTasks)))
	return replicationMessages, nil
}
