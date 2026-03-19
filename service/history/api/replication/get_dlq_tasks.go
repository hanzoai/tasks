package replication

import (
	"context"

	replicationspb "github.com/hanzoai/tasks/api/replication/v1"
	"github.com/hanzoai/tasks/common/log/tag"
	historyi "github.com/hanzoai/tasks/service/history/interfaces"
	"github.com/hanzoai/tasks/service/history/replication"
)

func GetDLQTasks(
	ctx context.Context,
	shard historyi.ShardContext,
	replicationAckMgr replication.AckManager,
	taskInfos []*replicationspb.ReplicationTaskInfo,
) ([]*replicationspb.ReplicationTask, error) {
	tasks := make([]*replicationspb.ReplicationTask, 0, len(taskInfos))
	for _, taskInfo := range taskInfos {
		task, err := replicationAckMgr.GetTask(ctx, taskInfo)
		if err != nil {
			shard.GetLogger().Error("Failed to fetch DLQ replication messages.", tag.Error(err))
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}
