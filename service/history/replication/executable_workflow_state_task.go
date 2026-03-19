package replication

import (
	"context"
	"errors"
	"time"

	"go.temporal.io/api/serviceerror"
	enumsspb "github.com/hanzoai/tasks/api/enums/v1"
	"github.com/hanzoai/tasks/api/historyservice/v1"
	persistencespb "github.com/hanzoai/tasks/api/persistence/v1"
	replicationspb "github.com/hanzoai/tasks/api/replication/v1"
	"github.com/hanzoai/tasks/common/definition"
	"github.com/hanzoai/tasks/common/headers"
	"github.com/hanzoai/tasks/common/log/tag"
	"github.com/hanzoai/tasks/common/metrics"
	"github.com/hanzoai/tasks/common/namespace"
	serviceerrors "github.com/hanzoai/tasks/common/serviceerror"
	ctasks "github.com/hanzoai/tasks/common/tasks"
	"github.com/hanzoai/tasks/service/history/consts"
)

type (
	ExecutableWorkflowStateTask struct {
		ProcessToolBox

		definition.WorkflowKey
		ExecutableTask
		req *historyservice.ReplicateWorkflowStateRequest
	}
)

var _ ctasks.Task = (*ExecutableWorkflowStateTask)(nil)
var _ TrackableExecutableTask = (*ExecutableWorkflowStateTask)(nil)

// TODO should workflow task be batched?

func NewExecutableWorkflowStateTask(
	processToolBox ProcessToolBox,
	taskID int64,
	taskCreationTime time.Time,
	task *replicationspb.SyncWorkflowStateTaskAttributes,
	sourceClusterName string,
	sourceShardKey ClusterShardKey,
	replicationTask *replicationspb.ReplicationTask,
) *ExecutableWorkflowStateTask {
	namespaceID := task.GetWorkflowState().ExecutionInfo.NamespaceId
	workflowID := task.GetWorkflowState().ExecutionInfo.WorkflowId
	runID := task.GetWorkflowState().ExecutionState.RunId
	return &ExecutableWorkflowStateTask{
		ProcessToolBox: processToolBox,

		WorkflowKey: definition.NewWorkflowKey(namespaceID, workflowID, runID),
		ExecutableTask: NewExecutableTask(
			processToolBox,
			taskID,
			metrics.SyncWorkflowStateTaskScope,
			taskCreationTime,
			time.Now().UTC(),
			sourceClusterName,
			sourceShardKey,
			replicationTask,
		),
		req: &historyservice.ReplicateWorkflowStateRequest{
			NamespaceId:              namespaceID,
			WorkflowState:            task.GetWorkflowState(),
			RemoteCluster:            sourceClusterName,
			IsForceReplication:       task.GetIsForceReplication(),
			IsCloseTransferTaskAcked: task.GetIsCloseTransferTaskAcked(),
		},
	}
}

func (e *ExecutableWorkflowStateTask) QueueID() any {
	return e.WorkflowKey
}

func (e *ExecutableWorkflowStateTask) Execute() error {
	if e.TerminalState() {
		return nil
	}
	e.MarkExecutionStart()

	callerInfo := getReplicaitonCallerInfo(e.GetPriority())
	namespaceName, apply, err := e.GetNamespaceInfo(headers.SetCallerInfo(
		context.Background(),
		callerInfo,
	), e.NamespaceID, e.WorkflowID)
	if err != nil {
		return err
	} else if !apply {
		e.Logger.Warn("Skipping the replication task",
			tag.WorkflowNamespaceID(e.NamespaceID),
			tag.WorkflowID(e.WorkflowID),
			tag.WorkflowRunID(e.RunID),
			tag.TaskID(e.ExecutableTask.TaskID()),
		)
		metrics.ReplicationTasksSkipped.With(e.MetricsHandler).Record(
			1,
			metrics.OperationTag(metrics.SyncWorkflowStateTaskScope),
			metrics.NamespaceTag(namespaceName),
		)
		return nil
	}
	ctx, cancel := newTaskContext(namespaceName, e.Config.ReplicationTaskApplyTimeout(), callerInfo)
	defer cancel()

	shardContext, err := e.ShardController.GetShardByNamespaceWorkflow(
		namespace.ID(e.NamespaceID),
		e.WorkflowID,
	)
	if err != nil {
		return err
	}
	engine, err := shardContext.GetEngine(ctx)
	if err != nil {
		return err
	}
	return engine.ReplicateWorkflowState(ctx, e.req)
}

func (e *ExecutableWorkflowStateTask) HandleErr(err error) error {
	metrics.ReplicationTasksErrorByType.With(e.MetricsHandler).Record(
		1,
		metrics.OperationTag(metrics.SyncWorkflowStateTaskScope),
		metrics.NamespaceTag(e.NamespaceName()),
		metrics.ServiceErrorTypeTag(err),
	)
	if errors.Is(err, consts.ErrDuplicate) {
		e.MarkTaskDuplicated()
		return nil
	}
	callerInfo := getReplicaitonCallerInfo(e.GetPriority())
	switch retryErr := err.(type) {
	case *serviceerrors.SyncState:
		namespaceName, _, nsError := e.GetNamespaceInfo(headers.SetCallerInfo(
			context.Background(),
			callerInfo,
		), e.NamespaceID, e.WorkflowID)
		if nsError != nil {
			return err
		}
		ctx, cancel := newTaskContext(namespaceName, e.Config.ReplicationTaskApplyTimeout(), callerInfo)
		defer cancel()

		if doContinue, syncStateErr := e.SyncState(
			ctx,
			retryErr,
			ResendAttempt,
		); syncStateErr != nil || !doContinue {
			if syncStateErr != nil {
				e.Logger.Error("SyncWorkflowState replication task encountered error during sync state",
					tag.WorkflowNamespaceID(e.NamespaceID),
					tag.WorkflowID(e.WorkflowID),
					tag.WorkflowRunID(e.RunID),
					tag.TaskID(e.ExecutableTask.TaskID()),
					tag.Error(syncStateErr),
				)
				return err
			}
			return nil
		}
		return nil
	case nil, *serviceerror.NotFound:
		return nil
	case *serviceerrors.RetryReplication:
		namespaceName, _, nsError := e.GetNamespaceInfo(headers.SetCallerInfo(
			context.Background(),
			callerInfo,
		), e.NamespaceID, e.WorkflowID)
		if nsError != nil {
			return err
		}
		ctx, cancel := newTaskContext(namespaceName, e.Config.ReplicationTaskApplyTimeout(), callerInfo)
		defer cancel()

		if doContinue, resendErr := e.Resend(
			ctx,
			e.ExecutableTask.SourceClusterName(),
			retryErr,
			ResendAttempt,
		); resendErr != nil || !doContinue {
			return err
		}
		return e.Execute()
	default:
		e.Logger.Error("workflow state replication task encountered error",
			tag.WorkflowNamespaceID(e.NamespaceID),
			tag.WorkflowID(e.WorkflowID),
			tag.WorkflowRunID(e.RunID),
			tag.TaskID(e.ExecutableTask.TaskID()),
			tag.Error(err),
		)
		return err
	}
}

func (e *ExecutableWorkflowStateTask) MarkPoisonPill() error {
	if e.ReplicationTask().GetRawTaskInfo() == nil {
		e.ReplicationTask().RawTaskInfo = &persistencespb.ReplicationTaskInfo{
			NamespaceId: e.NamespaceID,
			WorkflowId:  e.WorkflowID,
			RunId:       e.RunID,
			TaskId:      e.ExecutableTask.TaskID(),
			TaskType:    enumsspb.TASK_TYPE_REPLICATION_SYNC_WORKFLOW_STATE,
		}
	}

	return e.ExecutableTask.MarkPoisonPill()
}
