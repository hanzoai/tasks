package replication

import (
	"github.com/hanzoai/tasks/client"
	"github.com/hanzoai/tasks/common/cluster"
	"github.com/hanzoai/tasks/common/log"
	"github.com/hanzoai/tasks/common/metrics"
	"github.com/hanzoai/tasks/common/namespace"
	"github.com/hanzoai/tasks/common/persistence/serialization"
	ctasks "github.com/hanzoai/tasks/common/tasks"
	"github.com/hanzoai/tasks/service/history/configs"
	"github.com/hanzoai/tasks/service/history/replication/eventhandler"
	"github.com/hanzoai/tasks/service/history/shard"
	wcache "github.com/hanzoai/tasks/service/history/workflow/cache"
	"go.uber.org/fx"
)

type (
	ProcessToolBox struct {
		fx.In

		Config                    *configs.Config
		ClusterMetadata           cluster.Metadata
		ClientBean                client.Bean
		ShardController           shard.Controller
		NamespaceCache            namespace.Registry
		EagerNamespaceRefresher   EagerNamespaceRefresher
		ResendHandler             eventhandler.ResendHandler
		HighPriorityTaskScheduler ctasks.Scheduler[TrackableExecutableTask] `name:"HighPriorityTaskScheduler"`
		// consider using a single TaskScheduler i.e. InterleavedWeightedRoundRobinScheduler instead of two
		LowPriorityTaskScheduler ctasks.Scheduler[TrackableExecutableTask] `name:"LowPriorityTaskScheduler"`
		MetricsHandler           metrics.Handler
		Logger                   log.Logger
		ThrottledLogger          log.ThrottledLogger
		Serializer               serialization.Serializer
		DLQWriter                DLQWriter
		HistoryEventsHandler     eventhandler.HistoryEventsHandler
		WorkflowCache            wcache.Cache
		RemoteHistoryFetcher     eventhandler.HistoryPaginatedFetcher
	}
)
