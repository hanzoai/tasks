package listqueues

import (
	"context"
	"errors"

	"go.temporal.io/api/serviceerror"
	"github.com/hanzoai/tasks/api/historyservice/v1"
	"github.com/hanzoai/tasks/common/persistence"
	"github.com/hanzoai/tasks/service/history/consts"
)

func Invoke(
	ctx context.Context,
	historyTaskQueueManager persistence.HistoryTaskQueueManager,
	req *historyservice.ListQueuesRequest,
) (*historyservice.ListQueuesResponse, error) {
	resp, err := historyTaskQueueManager.ListQueues(ctx, &persistence.ListQueuesRequest{
		QueueType:     persistence.QueueV2Type(req.QueueType),
		PageSize:      int(req.PageSize),
		NextPageToken: req.NextPageToken,
	})
	if err != nil {
		if errors.Is(err, persistence.ErrNonPositiveListQueuesPageSize) {
			return nil, consts.ErrInvalidPageSize
		}
		if errors.Is(err, persistence.ErrNegativeListQueuesOffset) || errors.Is(err, persistence.ErrInvalidListQueuesNextPageToken) {
			return nil, consts.ErrInvalidPaginationToken
		}
		return nil, serviceerror.NewUnavailablef("ListQueues failed. Error: %v", err)
	}
	var queues []*historyservice.ListQueuesResponse_QueueInfo
	for _, queue := range resp.Queues {
		queues = append(queues, &historyservice.ListQueuesResponse_QueueInfo{
			QueueName:     queue.QueueName,
			MessageCount:  queue.MessageCount,
			LastMessageId: queue.LastMessageID,
		})
	}
	return &historyservice.ListQueuesResponse{
		Queues:        queues,
		NextPageToken: resp.NextPageToken,
	}, nil
}
