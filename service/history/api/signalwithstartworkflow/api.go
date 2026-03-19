package signalwithstartworkflow

import (
	"context"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"github.com/hanzoai/tasks/api/historyservice/v1"
	"github.com/hanzoai/tasks/api/matchingservice/v1"
	"github.com/hanzoai/tasks/common/definition"
	"github.com/hanzoai/tasks/common/enums"
	"github.com/hanzoai/tasks/common/locks"
	"github.com/hanzoai/tasks/common/metrics"
	"github.com/hanzoai/tasks/common/namespace"
	"github.com/hanzoai/tasks/common/worker_versioning"
	"github.com/hanzoai/tasks/service/history/api"
	historyi "github.com/hanzoai/tasks/service/history/interfaces"
)

func Invoke(
	ctx context.Context,
	signalWithStartRequest *historyservice.SignalWithStartWorkflowExecutionRequest,
	shard historyi.ShardContext,
	workflowConsistencyChecker api.WorkflowConsistencyChecker,
	matchingClient matchingservice.MatchingServiceClient,
	versionMembershipCache worker_versioning.VersionMembershipCache,
	reactivationSignalCache worker_versioning.ReactivationSignalCache,
	reactivationSignaler api.VersionReactivationSignalerFn,
) (_ *historyservice.SignalWithStartWorkflowExecutionResponse, retError error) {
	namespaceEntry, err := api.GetActiveNamespace(shard, namespace.ID(signalWithStartRequest.GetNamespaceId()), signalWithStartRequest.SignalWithStartRequest.WorkflowId)
	if err != nil {
		return nil, err
	}
	namespaceID := namespaceEntry.ID()

	var currentWorkflowLease api.WorkflowLease
	currentWorkflowLease, err = workflowConsistencyChecker.GetWorkflowLease(
		ctx,
		nil,
		definition.NewWorkflowKey(
			string(namespaceID),
			signalWithStartRequest.SignalWithStartRequest.WorkflowId,
			"",
		),
		locks.PriorityHigh,
	)
	switch err.(type) {
	case nil:
		defer func() { currentWorkflowLease.GetReleaseFn()(retError) }()
	case *serviceerror.NotFound:
		currentWorkflowLease = nil
	default:
		return nil, err
	}

	// TODO: remove this call in 1.25
	enums.SetDefaultWorkflowIdConflictPolicy(
		&signalWithStartRequest.SignalWithStartRequest.WorkflowIdConflictPolicy,
		enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING)

	api.MigrateWorkflowIdReusePolicyForRunningWorkflow(
		&signalWithStartRequest.SignalWithStartRequest.WorkflowIdReusePolicy,
		&signalWithStartRequest.SignalWithStartRequest.WorkflowIdConflictPolicy)

	startRequest := ConvertToStartRequest(
		namespaceID,
		signalWithStartRequest.SignalWithStartRequest,
		shard.GetTimeSource().Now(),
	)
	request := startRequest.StartRequest

	api.OverrideStartWorkflowExecutionRequest(request, metrics.HistorySignalWithStartWorkflowExecutionScope, shard, shard.GetMetricsHandler())

	err = api.ValidateStartWorkflowExecutionRequest(ctx, request, shard, namespaceEntry, "SignalWithStartWorkflowExecution")
	if err != nil {
		return nil, err
	}

	// Validation for versioning override, if any.
	err = worker_versioning.ValidateVersioningOverride(ctx, request.GetVersioningOverride(), matchingClient, versionMembershipCache, request.GetTaskQueue().GetName(), enumspb.TASK_QUEUE_TYPE_WORKFLOW, namespaceID.String())
	if err != nil {
		return nil, err
	}

	runID, started, err := SignalWithStartWorkflow(
		ctx,
		shard,
		namespaceEntry,
		currentWorkflowLease,
		startRequest,
		signalWithStartRequest.SignalWithStartRequest,
	)
	if err != nil {
		return nil, err
	}

	// Notify version workflow if we're starting a new workflow pinned to a potentially drained version
	if started {
		api.ReactivateVersionWorkflowIfPinned(ctx, namespaceEntry, request.GetVersioningOverride(), reactivationSignalCache, reactivationSignaler, shard.GetConfig().EnableVersionReactivationSignals())
	}

	return &historyservice.SignalWithStartWorkflowExecutionResponse{
		RunId:   runID,
		Started: started,
	}, nil
}
