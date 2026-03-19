package removesignalmutablestate

import (
	"context"

	"github.com/hanzoai/tasks/api/historyservice/v1"
	"github.com/hanzoai/tasks/common/definition"
	"github.com/hanzoai/tasks/common/namespace"
	"github.com/hanzoai/tasks/service/history/api"
	"github.com/hanzoai/tasks/service/history/consts"
	historyi "github.com/hanzoai/tasks/service/history/interfaces"
)

func Invoke(
	ctx context.Context,
	req *historyservice.RemoveSignalMutableStateRequest,
	shard historyi.ShardContext,
	workflowConsistencyChecker api.WorkflowConsistencyChecker,
) (resp *historyservice.RemoveSignalMutableStateResponse, retError error) {
	_, err := api.GetActiveNamespace(shard, namespace.ID(req.GetNamespaceId()), req.WorkflowExecution.WorkflowId)
	if err != nil {
		return nil, err
	}

	err = api.GetAndUpdateWorkflowWithNew(
		ctx,
		nil,
		definition.NewWorkflowKey(
			req.NamespaceId,
			req.WorkflowExecution.WorkflowId,
			req.WorkflowExecution.RunId,
		),
		func(workflowLease api.WorkflowLease) (*api.UpdateWorkflowAction, error) {
			mutableState := workflowLease.GetMutableState()
			if !mutableState.IsWorkflowExecutionRunning() {
				return nil, consts.ErrWorkflowCompleted
			}

			mutableState.DeleteSignalRequested(req.GetRequestId())
			return &api.UpdateWorkflowAction{
				Noop:               false,
				CreateWorkflowTask: false,
			}, nil
		},
		nil,
		shard,
		workflowConsistencyChecker,
	)
	if err != nil {
		return nil, err
	}
	return &historyservice.RemoveSignalMutableStateResponse{}, nil
}
