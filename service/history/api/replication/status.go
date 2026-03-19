package replication

import (
	"context"

	"github.com/hanzoai/tasks/api/historyservice/v1"
	historyi "github.com/hanzoai/tasks/service/history/interfaces"
	"github.com/hanzoai/tasks/service/history/replication"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func GetStatus(
	ctx context.Context,
	request *historyservice.GetReplicationStatusRequest,
	shard historyi.ShardContext,
	replicationAckMgr replication.AckManager,
) (_ *historyservice.ShardReplicationStatus, retError error) {
	resp := &historyservice.ShardReplicationStatus{
		ShardId:        shard.GetShardID(),
		ShardLocalTime: timestamppb.New(shard.GetTimeSource().Now()),
	}

	maxReplicationTaskId, maxTaskVisibilityTimeStamp := replicationAckMgr.GetMaxTaskInfo()
	resp.MaxReplicationTaskId = maxReplicationTaskId
	resp.MaxReplicationTaskVisibilityTime = timestamppb.New(maxTaskVisibilityTimeStamp)

	remoteClusters, handoverNamespaces, err := shard.GetReplicationStatus(request.RemoteClusters)
	if err != nil {
		return nil, err
	}
	resp.RemoteClusters = remoteClusters
	resp.HandoverNamespaces = handoverNamespaces
	return resp, nil
}
