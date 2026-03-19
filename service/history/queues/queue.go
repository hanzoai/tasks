package queues

import (
	"github.com/hanzoai/tasks/service/history/tasks"
)

//go:generate mockgen -package $GOPACKAGE -source $GOFILE -destination queue_mock.go

type (
	Queue interface {
		Category() tasks.Category
		NotifyNewTasks(tasks []tasks.Task)
		FailoverNamespace(namespaceID string)
		Start()
		Stop()
	}
)
