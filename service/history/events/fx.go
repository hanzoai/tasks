package events

import (
	"github.com/hanzoai/tasks/common/log"
	"github.com/hanzoai/tasks/common/metrics"
	"github.com/hanzoai/tasks/common/persistence"
	"github.com/hanzoai/tasks/service/history/configs"
	"go.uber.org/fx"
)

var Module = fx.Options(
	fx.Provide(func(executionManager persistence.ExecutionManager, config *configs.Config, handler metrics.Handler, logger log.Logger) Cache {
		return NewHostLevelEventsCache(executionManager, config, handler, logger, false)
	}),
)
