package worker

import (
	"context"
	"os"

	"github.com/hanzoai/tasks/api/adminservice/v1"
	"github.com/hanzoai/tasks/chasm"
	schedulerpb "github.com/hanzoai/tasks/chasm/lib/scheduler/gen/schedulerpb/v1"
	"github.com/hanzoai/tasks/client"
	"github.com/hanzoai/tasks/common"
	"github.com/hanzoai/tasks/common/cluster"
	"github.com/hanzoai/tasks/common/config"
	"github.com/hanzoai/tasks/common/dynamicconfig"
	"github.com/hanzoai/tasks/common/log"
	"github.com/hanzoai/tasks/common/log/tag"
	"github.com/hanzoai/tasks/common/membership"
	"github.com/hanzoai/tasks/common/metrics"
	"github.com/hanzoai/tasks/common/namespace"
	"github.com/hanzoai/tasks/common/namespace/nsreplication"
	"github.com/hanzoai/tasks/common/persistence"
	"github.com/hanzoai/tasks/common/persistence/serialization"
	"github.com/hanzoai/tasks/common/persistence/visibility"
	"github.com/hanzoai/tasks/common/persistence/visibility/manager"
	"github.com/hanzoai/tasks/common/primitives"
	"github.com/hanzoai/tasks/common/resolver"
	"github.com/hanzoai/tasks/common/resource"
	"github.com/hanzoai/tasks/common/sdk"
	"github.com/hanzoai/tasks/common/searchattribute"
	"github.com/hanzoai/tasks/service"
	"github.com/hanzoai/tasks/service/worker/batcher"
	workercommon "github.com/hanzoai/tasks/service/worker/common"
	"github.com/hanzoai/tasks/service/worker/deletenamespace"
	"github.com/hanzoai/tasks/service/worker/dlq"
	"github.com/hanzoai/tasks/service/worker/dummy"
	"github.com/hanzoai/tasks/service/worker/migration"
	"github.com/hanzoai/tasks/service/worker/scheduler"
	"github.com/hanzoai/tasks/service/worker/workerdeployment"
	"go.uber.org/fx"
	"google.golang.org/grpc"
)

var Module = fx.Options(
	migration.Module,
	resource.Module,
	deletenamespace.Module,
	scheduler.Module,
	batcher.Module,
	workerdeployment.Module,
	dlq.Module,
	dummy.Module,
	fx.Provide(schedulerpb.NewSchedulerServiceLayeredClient),
	fx.Provide(
		func(c resource.HistoryClient) dlq.HistoryClient {
			return c
		},
		func(m cluster.Metadata) dlq.CurrentClusterName {
			return dlq.CurrentClusterName(m.GetCurrentClusterName())
		},
		func(b client.Bean) dlq.TaskClientDialer {
			return dlq.TaskClientDialerFn(func(_ context.Context, address string) (dlq.TaskClient, error) {
				c, err := b.GetRemoteAdminClient(address)
				if err != nil {
					return nil, err
				}
				return dlq.AddTasksFn(func(
					ctx context.Context,
					req *adminservice.AddTasksRequest,
				) (*adminservice.AddTasksResponse, error) {
					return c.AddTasks(ctx, req)
				}), nil
			})
		},
	),
	fx.Provide(HostInfoProvider),
	fx.Provide(VisibilityManagerProvider),
	fx.Provide(ThrottledLoggerRpsFnProvider),
	fx.Provide(ConfigProvider),
	fx.Provide(PersistenceRateLimitingParamsProvider),
	service.PersistenceLazyLoadedServiceResolverModule,
	fx.Provide(ServiceResolverProvider),
	fx.Provide(func(
		clusterMetadata cluster.Metadata,
		metadataManager persistence.MetadataManager,
		dataMerger nsreplication.NamespaceDataMerger,
		logger log.Logger,
	) nsreplication.TaskExecutor {
		return nsreplication.NewTaskExecutor(
			clusterMetadata.GetCurrentClusterName(),
			metadataManager,
			dataMerger,
			logger,
		)
	}),
	fx.Provide(nsreplication.NewNoopDataMerger),
	fx.Provide(ServerProvider),
	fx.Provide(NewService),
	fx.Provide(fx.Annotate(NewWorkerManager, fx.ParamTags(workercommon.WorkerComponentTag))),
	fx.Provide(PerNamespaceWorkerManagerProvider),
	fx.Invoke(ServiceLifetimeHooks),
)

func ThrottledLoggerRpsFnProvider(serviceConfig *Config) resource.ThrottledLoggerRpsFn {
	return func() float64 { return float64(serviceConfig.ThrottledLogRPS()) }
}

func PersistenceRateLimitingParamsProvider(
	serviceConfig *Config,
	persistenceLazyLoadedServiceResolver service.PersistenceLazyLoadedServiceResolver,
	logger log.SnTaggedLogger,
) service.PersistenceRateLimitingParams {
	return service.NewPersistenceRateLimitingParams(
		serviceConfig.PersistenceMaxQPS,
		serviceConfig.PersistenceGlobalMaxQPS,
		serviceConfig.PersistenceNamespaceMaxQPS,
		serviceConfig.PersistenceGlobalNamespaceMaxQPS,
		serviceConfig.PersistencePerShardNamespaceMaxQPS,
		serviceConfig.OperatorRPSRatio,
		serviceConfig.PersistenceQPSBurstRatio,
		serviceConfig.PersistenceDynamicRateLimitingParams,
		persistenceLazyLoadedServiceResolver,
		logger,
	)
}

func HostInfoProvider() (membership.HostInfo, error) {
	hn, err := os.Hostname()
	return membership.NewHostInfoFromAddress(hn), err
}

func ServiceResolverProvider(
	membershipMonitor membership.Monitor,
) (membership.ServiceResolver, error) {
	return membershipMonitor.GetResolver(primitives.WorkerService)
}

func ConfigProvider(
	dc *dynamicconfig.Collection,
	persistenceConfig *config.Persistence,
) *Config {
	return NewConfig(
		dc,
		persistenceConfig,
	)
}

func VisibilityManagerProvider(
	logger log.Logger,
	metricsHandler metrics.Handler,
	persistenceConfig *config.Persistence,
	customVisibilityStoreFactory visibility.VisibilityStoreFactory,
	serviceConfig *Config,
	persistenceServiceResolver resolver.ServiceResolver,
	searchAttributesMapperProvider searchattribute.MapperProvider,
	saProvider searchattribute.Provider,
	namespaceRegistry namespace.Registry,
	chasmRegistry *chasm.Registry,
	serializer serialization.Serializer,
) (manager.VisibilityManager, error) {
	return visibility.NewManager(
		*persistenceConfig,
		persistenceServiceResolver,
		customVisibilityStoreFactory,
		nil, // worker visibility never write
		saProvider,
		searchAttributesMapperProvider,
		namespaceRegistry,
		chasmRegistry,
		serviceConfig.VisibilityPersistenceMaxReadQPS,
		serviceConfig.VisibilityPersistenceMaxWriteQPS,
		serviceConfig.OperatorRPSRatio,
		serviceConfig.VisibilityPersistenceSlowQueryThreshold,
		serviceConfig.EnableReadFromSecondaryVisibility,
		serviceConfig.VisibilityEnableShadowReadMode,
		dynamicconfig.GetStringPropertyFn(visibility.SecondaryVisibilityWritingModeOff), // worker visibility never write
		serviceConfig.VisibilityDisableOrderByClause,
		serviceConfig.VisibilityEnableManualPagination,
		serviceConfig.VisibilityEnableUnifiedQueryConverter,
		metricsHandler,
		logger,
		serializer,
	)
}

func ServiceLifetimeHooks(lc fx.Lifecycle, svc *Service) {
	lc.Append(fx.StartStopHook(svc.Start, svc.Stop))
}

type perNamespaceWorkerManagerInitParams struct {
	fx.In
	Logger            log.Logger
	SdkClientFactory  sdk.ClientFactory
	NamespaceRegistry namespace.Registry
	HostName          resource.HostName
	Config            *Config
	ClusterMetadata   cluster.Metadata
	Components        []workercommon.PerNSWorkerComponent `group:"perNamespaceWorkerComponent"`
}

func PerNamespaceWorkerManagerProvider(params perNamespaceWorkerManagerInitParams) *PerNamespaceWorkerManager {
	return NewPerNamespaceWorkerManager(
		params.Logger,
		params.SdkClientFactory,
		params.NamespaceRegistry,
		params.HostName,
		params.Config,
		params.ClusterMetadata,
		params.Components,
		primitives.PerNSWorkerTaskQueue,
	)
}

func ServerProvider(rpcFactory common.RPCFactory, logger log.Logger) *grpc.Server {
	opts, err := rpcFactory.GetInternodeGRPCServerOptions()
	if err != nil {
		logger.Fatal("Failed to get gRPC server options", tag.Error(err))
	}
	return grpc.NewServer(opts...)
}
