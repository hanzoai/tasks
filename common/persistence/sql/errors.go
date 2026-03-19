package sql

import (
	enumspb "go.temporal.io/api/enums/v1"
	enumsspb "github.com/hanzoai/tasks/api/enums/v1"
	p "github.com/hanzoai/tasks/common/persistence"
	"github.com/hanzoai/tasks/common/persistence/sql/sqlplugin"
)

func (m *sqlExecutionStore) extractCurrentWorkflowConflictError(
	currentRow *sqlplugin.CurrentExecutionsRow,
	message string,
) error {
	if currentRow == nil {
		return &p.CurrentWorkflowConditionFailedError{
			Msg:              message,
			RequestIDs:       nil,
			RunID:            "",
			State:            enumsspb.WORKFLOW_EXECUTION_STATE_UNSPECIFIED,
			Status:           enumspb.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED,
			LastWriteVersion: 0,
			StartTime:        nil,
		}
	}

	executionState, err := workflowExecutionStateFromCurrentExecutionsRow(m.serializer, currentRow)
	if err != nil {
		return err
	}

	return &p.CurrentWorkflowConditionFailedError{
		Msg:              message,
		RequestIDs:       executionState.RequestIds,
		RunID:            currentRow.RunID.String(),
		State:            currentRow.State,
		Status:           currentRow.Status,
		LastWriteVersion: currentRow.LastWriteVersion,
		StartTime:        currentRow.StartTime,
	}
}
