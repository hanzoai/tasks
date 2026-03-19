package deletedlqtasks

import (
	"context"

	"go.temporal.io/api/serviceerror"
	"github.com/hanzoai/tasks/api/historyservice/v1"
	"github.com/hanzoai/tasks/common/persistence"
	"github.com/hanzoai/tasks/service/history/api"
	"github.com/hanzoai/tasks/service/history/tasks"
)

func Invoke(
	ctx context.Context,
	historyTaskQueueManager persistence.HistoryTaskQueueManager,
	req *historyservice.DeleteDLQTasksRequest,
	registry tasks.TaskCategoryRegistry,
) (*historyservice.DeleteDLQTasksResponse, error) {
	category, err := api.GetTaskCategory(int(req.DlqKey.TaskCategory), registry)
	if err != nil {
		return nil, err
	}

	if req.InclusiveMaxTaskMetadata == nil {
		return nil, serviceerror.NewInvalidArgument("must supply inclusive_max_task_metadata")
	}

	resp, err := historyTaskQueueManager.DeleteTasks(ctx, &persistence.DeleteTasksRequest{
		QueueKey: persistence.QueueKey{
			QueueType:     persistence.QueueTypeHistoryDLQ,
			Category:      category,
			SourceCluster: req.DlqKey.SourceCluster,
			TargetCluster: req.DlqKey.TargetCluster,
		},
		InclusiveMaxMessageMetadata: persistence.MessageMetadata{
			ID: req.InclusiveMaxTaskMetadata.MessageId,
		},
	})
	if err != nil {
		return nil, err
	}

	return &historyservice.DeleteDLQTasksResponse{MessagesDeleted: resp.MessagesDeleted}, nil
}
