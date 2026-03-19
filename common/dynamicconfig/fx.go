package dynamicconfig

import (
	"github.com/hanzoai/tasks/common/log"
	"github.com/hanzoai/tasks/common/pingable"
	"go.uber.org/fx"
)

var Module = fx.Options(
	fx.Provide(func(client Client, logger log.Logger, lc fx.Lifecycle) *Collection {
		col := NewCollection(client, logger)
		lc.Append(fx.StartStopHook(col.Start, col.Stop))
		return col
	}),
	fx.Provide(fx.Annotate(
		func(c *Collection) pingable.Pingable { return c },
		fx.ResultTags(`group:"deadlockDetectorRoots"`),
	)),
)
