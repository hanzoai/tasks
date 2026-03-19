package queues

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/hanzoai/tasks/service/history/tasks"
)

func TestGetArchivalTaskTypeTagValue(t *testing.T) {
	assert.Equal(t, "ArchivalTaskArchiveExecution", GetArchivalTaskTypeTagValue(&tasks.ArchiveExecutionTask{}))

	unknownTask := &tasks.CloseExecutionTask{}
	assert.Equal(t, unknownTask.GetType().String(), GetArchivalTaskTypeTagValue(unknownTask))
}
