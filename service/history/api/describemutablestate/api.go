package describemutablestate

import (
	"context"

	"github.com/hanzoai/tasks/api/historyservice/v1"
	"github.com/hanzoai/tasks/chasm"
	"github.com/hanzoai/tasks/common/definition"
	"github.com/hanzoai/tasks/common/locks"
	"github.com/hanzoai/tasks/common/namespace"
	"github.com/hanzoai/tasks/service/history/api"
	historyi "github.com/hanzoai/tasks/service/history/interfaces"
	"github.com/hanzoai/tasks/service/history/workflow"
)

func Invoke(
	ctx context.Context,
	req *historyservice.DescribeMutableStateRequest,
	shardContext historyi.ShardContext,
	workflowConsistencyChecker api.WorkflowConsistencyChecker,
) (_ *historyservice.DescribeMutableStateResponse, retError error) {
	namespaceID := namespace.ID(req.GetNamespaceId())
	err := api.ValidateNamespaceUUID(namespaceID)
	if err != nil {
		return nil, err
	}

	archetypeID := req.GetArchetypeId()
	if archetypeID == chasm.UnspecifiedArchetypeID {
		archetypeID = chasm.WorkflowArchetypeID
	}

	chasmLease, err := workflowConsistencyChecker.GetChasmLease(
		ctx,
		nil,
		definition.NewWorkflowKey(
			req.NamespaceId,
			req.Execution.WorkflowId,
			req.Execution.RunId,
		),
		archetypeID,
		locks.PriorityHigh,
	)
	if err != nil {
		return nil, err
	}
	defer func() { chasmLease.GetReleaseFn()(retError) }()

	response := &historyservice.DescribeMutableStateResponse{}
	if chasmLease.GetContext().(*workflow.ContextImpl).MutableState != nil {
		msb := chasmLease.GetContext().(*workflow.ContextImpl).MutableState
		response.CacheMutableState = msb.CloneToProto()
	}

	if !req.GetSkipForceReload() {
		// Clear mutable state to force reload from persistence.
		chasmLease.GetContext().Clear()
	}

	mutableState, err := chasmLease.GetContext().LoadMutableState(ctx, shardContext)
	if err != nil {
		return nil, err
	}

	response.DatabaseMutableState = mutableState.CloneToProto()
	return response, nil
}
