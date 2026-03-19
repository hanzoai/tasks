package history

import (
	"go.opentelemetry.io/otel/trace"
	"github.com/hanzoai/tasks/chasm"
	"github.com/hanzoai/tasks/client"
	"github.com/hanzoai/tasks/common/persistence"
	"github.com/hanzoai/tasks/common/persistence/serialization"
	"github.com/hanzoai/tasks/common/persistence/visibility/manager"
	"github.com/hanzoai/tasks/common/resource"
	"github.com/hanzoai/tasks/common/sdk"
	"github.com/hanzoai/tasks/common/testing/testhooks"
	"github.com/hanzoai/tasks/common/worker_versioning"
	"github.com/hanzoai/tasks/service/history/api"
	"github.com/hanzoai/tasks/service/history/circuitbreakerpool"
	"github.com/hanzoai/tasks/service/history/configs"
	"github.com/hanzoai/tasks/service/history/events"
	historyi "github.com/hanzoai/tasks/service/history/interfaces"
	"github.com/hanzoai/tasks/service/history/replication"
	"github.com/hanzoai/tasks/service/history/tasks"
	"github.com/hanzoai/tasks/service/history/workflow"
	wcache "github.com/hanzoai/tasks/service/history/workflow/cache"
	"github.com/hanzoai/tasks/service/worker/workerdeployment"
	"go.uber.org/fx"
)

type (
	HistoryEngineFactoryParams struct {
		fx.In

		ClientBean                      client.Bean
		MatchingClient                  resource.MatchingClient
		SdkClientFactory                sdk.ClientFactory
		EventNotifier                   events.Notifier
		Config                          *configs.Config
		RawMatchingClient               resource.MatchingRawClient
		WorkflowCache                   wcache.Cache
		ReplicationProgressCache        replication.ProgressCache
		Serializer                      serialization.Serializer
		QueueFactories                  []QueueFactory `group:"queueFactory"`
		ReplicationTaskFetcherFactory   replication.TaskFetcherFactory
		ReplicationTaskExecutorProvider replication.TaskExecutorProvider
		TracerProvider                  trace.TracerProvider
		PersistenceVisibilityMgr        manager.VisibilityManager
		EventBlobCache                  persistence.XDCCache
		TaskCategoryRegistry            tasks.TaskCategoryRegistry
		ReplicationDLQWriter            replication.DLQWriter
		CommandHandlerRegistry          *workflow.CommandHandlerRegistry
		OutboundQueueCBPool             *circuitbreakerpool.OutboundQueueCircuitBreakerPool
		PersistenceRateLimiter          replication.PersistenceRateLimiter
		TestHooks                       testhooks.TestHooks
		ChasmEngine                     chasm.Engine
		VersionMembershipCache          worker_versioning.VersionMembershipCache
		ReactivationSignalCache         worker_versioning.ReactivationSignalCache
		WorkerDeploymentClient          workerdeployment.Client
		RoutingInfoCache                worker_versioning.RoutingInfoCache
	}

	historyEngineFactory struct {
		HistoryEngineFactoryParams
	}
)

func (f *historyEngineFactory) CreateEngine(
	shard historyi.ShardContext,
) historyi.Engine {
	return NewEngineWithShardContext(
		shard,
		f.ClientBean,
		f.MatchingClient,
		f.SdkClientFactory,
		f.EventNotifier,
		f.Config,
		f.VersionMembershipCache,
		f.ReactivationSignalCache,
		f.WorkerDeploymentClient,
		f.RoutingInfoCache,
		f.RawMatchingClient,
		f.WorkflowCache,
		f.ReplicationProgressCache,
		f.Serializer,
		f.QueueFactories,
		f.ReplicationTaskFetcherFactory,
		f.ReplicationTaskExecutorProvider,
		api.NewWorkflowConsistencyChecker(shard, f.WorkflowCache),
		f.TracerProvider,
		f.PersistenceVisibilityMgr,
		f.EventBlobCache,
		f.TaskCategoryRegistry,
		f.ReplicationDLQWriter,
		f.CommandHandlerRegistry,
		f.OutboundQueueCBPool,
		f.PersistenceRateLimiter,
		f.TestHooks,
		f.ChasmEngine,
	)
}
