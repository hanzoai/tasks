package history

import (
	"context"
	"time"

	"github.com/nexus-rpc/sdk-go/nexus"
	"github.com/hanzoai/tasks/api/historyservice/v1"
	"github.com/hanzoai/tasks/chasm"
	"github.com/hanzoai/tasks/chasm/lib/activity"
	"github.com/hanzoai/tasks/common"
	commoncache "github.com/hanzoai/tasks/common/cache"
	"github.com/hanzoai/tasks/common/clock"
	"github.com/hanzoai/tasks/common/config"
	"github.com/hanzoai/tasks/common/dynamicconfig"
	"github.com/hanzoai/tasks/common/log"
	"github.com/hanzoai/tasks/common/log/tag"
	"github.com/hanzoai/tasks/common/membership"
	"github.com/hanzoai/tasks/common/metrics"
	"github.com/hanzoai/tasks/common/namespace"
	persistenceClient "github.com/hanzoai/tasks/common/persistence/client"
	"github.com/hanzoai/tasks/common/persistence/serialization"
	"github.com/hanzoai/tasks/common/persistence/visibility"
	"github.com/hanzoai/tasks/common/persistence/visibility/manager"
	"github.com/hanzoai/tasks/common/persistence/visibility/store/elasticsearch"
	"github.com/hanzoai/tasks/common/primitives"
	"github.com/hanzoai/tasks/common/quotas/calculator"
	"github.com/hanzoai/tasks/common/resolver"
	"github.com/hanzoai/tasks/common/resource"
	"github.com/hanzoai/tasks/common/rpc/interceptor"
	"github.com/hanzoai/tasks/common/searchattribute"
	"github.com/hanzoai/tasks/common/tasktoken"
	"github.com/hanzoai/tasks/common/worker_versioning"
	"github.com/hanzoai/tasks/components/callbacks"
	"github.com/hanzoai/tasks/components/nexusoperations"
	nexusworkflow "github.com/hanzoai/tasks/components/nexusoperations/workflow"
	"github.com/hanzoai/tasks/service"
	"github.com/hanzoai/tasks/service/history/api"
	"github.com/hanzoai/tasks/service/history/archival"
	"github.com/hanzoai/tasks/service/history/configs"
	"github.com/hanzoai/tasks/service/history/consts"
	"github.com/hanzoai/tasks/service/history/events"
	"github.com/hanzoai/tasks/service/history/hsm"
	"github.com/hanzoai/tasks/service/history/replication"
	"github.com/hanzoai/tasks/service/history/shard"
	"github.com/hanzoai/tasks/service/history/workflow"
	"github.com/hanzoai/tasks/service/history/workflow/cache"
	"github.com/hanzoai/tasks/service/worker/workerdeployment"
	"go.uber.org/fx"
	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

var Module = fx.Options(
	resource.Module,
	fx.Provide(hsm.NewRegistry),
	workflow.Module,
	shard.Module,
	events.Module,
	cache.Module,
	archival.Module,
	ChasmEngineModule,
	fx.Provide(ConfigProvider), // might be worth just using provider for configs.Config directly
	fx.Provide(workflow.NewCommandHandlerRegistry),
	fx.Provide(RetryableInterceptorProvider),
	fx.Provide(ErrorHandlerProvider),
	fx.Provide(TelemetryInterceptorProvider),
	fx.Provide(RateLimitInterceptorProvider),
	fx.Provide(HealthSignalAggregatorProvider),
	fx.Provide(HealthCheckInterceptorProvider),
	fx.Provide(MetadataContextInterceptorProvider),
	fx.Provide(chasm.ChasmEngineInterceptorProvider),
	fx.Provide(chasm.ChasmVisibilityInterceptorProvider),
	fx.Provide(HistoryAdditionalInterceptorsProvider),
	fx.Provide(service.GrpcServerOptionsProvider),
	fx.Provide(ESProcessorConfigProvider),
	fx.Provide(VisibilityManagerProvider),
	fx.Provide(visibility.ChasmVisibilityManagerProvider),
	fx.Provide(ThrottledLoggerRpsFnProvider),
	fx.Provide(PersistenceRateLimitingParamsProvider),
	service.PersistenceLazyLoadedServiceResolverModule,
	fx.Provide(ServiceResolverProvider),
	fx.Provide(EventNotifierProvider),
	fx.Provide(HistoryEngineFactoryProvider),
	fx.Provide(HandlerProvider),
	fx.Provide(ServerProvider),
	fx.Provide(NewService),
	fx.Provide(ReplicationProgressCacheProvider),
	fx.Provide(VersionMembershipCacheProvider),
	fx.Provide(ReactivationSignalCacheProvider),
	fx.Provide(workerdeployment.ClientProvider),
	fx.Provide(RoutingInfoCacheProvider),
	fx.Invoke(ServiceLifetimeHooks),

	callbacks.Module,
	nexusoperations.Module,
	fx.Invoke(nexusworkflow.RegisterCommandHandlers),
	activity.HistoryModule,
)

func ServerProvider(grpcServerOptions []grpc.ServerOption) *grpc.Server {
	return grpc.NewServer(grpcServerOptions...)
}

func ServiceResolverProvider(
	membershipMonitor membership.Monitor,
) (membership.ServiceResolver, error) {
	return membershipMonitor.GetResolver(primitives.HistoryService)
}

func HandlerProvider(args NewHandlerArgs) (*Handler, error) {
	// Build and store the Nexus handler
	nexusHandler, err := buildNexusHandler(args.ChasmRegistry)
	if err != nil {
		return nil, err
	}

	handler := &Handler{
		status:                       common.DaemonStatusInitialized,
		config:                       args.Config,
		tokenSerializer:              tasktoken.NewSerializer(),
		logger:                       args.Logger,
		throttledLogger:              args.ThrottledLogger,
		persistenceExecutionManager:  args.PersistenceExecutionManager,
		persistenceShardManager:      args.PersistenceShardManager,
		persistenceVisibilityManager: args.PersistenceVisibilityManager,
		persistenceHealthSignal:      args.PersistenceHealthSignal,
		healthServer:                 args.HealthServer,
		historyHealthSignal:          args.HistoryHealthSignal,
		historyServiceResolver:       args.HistoryServiceResolver,
		metricsHandler:               args.MetricsHandler,
		payloadSerializer:            args.PayloadSerializer,
		timeSource:                   args.TimeSource,
		namespaceRegistry:            args.NamespaceRegistry,
		saProvider:                   args.SaProvider,
		clusterMetadata:              args.ClusterMetadata,
		archivalMetadata:             args.ArchivalMetadata,
		hostInfoProvider:             args.HostInfoProvider,
		controller:                   args.ShardController,
		eventNotifier:                args.EventNotifier,
		tracer:                       args.TracerProvider.Tracer(consts.LibraryName),
		taskQueueManager:             args.TaskQueueManager,
		taskCategoryRegistry:         args.TaskCategoryRegistry,
		dlqMetricsEmitter:            args.DLQMetricsEmitter,
		chasmEngine:                  args.ChasmEngine,
		chasmRegistry:                args.ChasmRegistry,

		replicationTaskFetcherFactory:    args.ReplicationTaskFetcherFactory,
		replicationTaskConverterProvider: args.ReplicationTaskConverterFactory,
		streamReceiverMonitor:            args.StreamReceiverMonitor,
		replicationServerRateLimiter:     args.ReplicationServerRateLimiter,
		nexusHandler:                     nexusHandler,
	}

	return handler, nil
}

func buildNexusHandler(chasmRegistry *chasm.Registry) (nexus.Handler, error) {
	nexusServices := chasmRegistry.NexusServices()
	if len(nexusServices) == 0 {
		return nil, nil
	}
	serviceRegistry := nexus.NewServiceRegistry()
	for _, svc := range nexusServices {
		// No chance of collision here since the registry would have errored out earlier.
		serviceRegistry.MustRegister(svc)
	}

	return serviceRegistry.NewHandler()
}

func HistoryEngineFactoryProvider(
	params HistoryEngineFactoryParams,
) shard.EngineFactory {
	return &historyEngineFactory{
		HistoryEngineFactoryParams: params,
	}
}

func ConfigProvider(
	dc *dynamicconfig.Collection,
	persistenceConfig config.Persistence,
) *configs.Config {
	return configs.NewConfig(
		dc,
		persistenceConfig.NumHistoryShards,
	)
}

func ThrottledLoggerRpsFnProvider(serviceConfig *configs.Config) resource.ThrottledLoggerRpsFn {
	return func() float64 { return float64(serviceConfig.ThrottledLogRPS()) }
}

func RetryableInterceptorProvider() *interceptor.RetryableInterceptor {
	return interceptor.NewRetryableInterceptor(
		common.CreateHistoryHandlerRetryPolicy(),
		api.IsRetryableError,
	)
}

func ErrorHandlerProvider(
	logger log.Logger,
	serviceConfig *configs.Config,
) *interceptor.RequestErrorHandler {
	return interceptor.NewRequestErrorHandler(
		logger,
		serviceConfig.LogAllReqErrors,
	)
}

func TelemetryInterceptorProvider(
	logger log.Logger,
	namespaceRegistry namespace.Registry,
	metricsHandler metrics.Handler,
	serviceConfig *configs.Config,
	requestErrorHandler *interceptor.RequestErrorHandler,
) *interceptor.TelemetryInterceptor {
	return interceptor.NewTelemetryInterceptor(
		namespaceRegistry,
		metricsHandler,
		logger,
		serviceConfig.LogAllReqErrors,
		requestErrorHandler,
	)
}

func HealthSignalAggregatorProvider(
	dynamicCollection *dynamicconfig.Collection,
	logger log.ThrottledLogger,
) interceptor.HealthSignalAggregator {
	return interceptor.NewHealthSignalAggregator(
		logger,
		dynamicconfig.HistoryHealthSignalMetricsEnabled.Get(dynamicCollection),
		dynamicconfig.PersistenceHealthSignalWindowSize.Get(dynamicCollection)(),
		dynamicconfig.PersistenceHealthSignalBufferSize.Get(dynamicCollection)(),
	)
}

func HealthCheckInterceptorProvider(
	healthSignalAggregator interceptor.HealthSignalAggregator,
) *interceptor.HealthCheckInterceptor {
	return interceptor.NewHealthCheckInterceptor(
		healthSignalAggregator,
	)
}

func MetadataContextInterceptorProvider() *interceptor.MetadataContextInterceptor {
	return interceptor.NewMetadataContextInterceptor()
}

func HistoryAdditionalInterceptorsProvider(
	healthCheckInterceptor *interceptor.HealthCheckInterceptor,
	metadataContextInterceptor *interceptor.MetadataContextInterceptor,
	chasmRequestEngineInterceptor *chasm.ChasmEngineInterceptor,
	chasmRequestVisibilityInterceptor *chasm.ChasmVisibilityInterceptor,
) []grpc.UnaryServerInterceptor {
	return []grpc.UnaryServerInterceptor{
		metadataContextInterceptor.Intercept,
		healthCheckInterceptor.UnaryIntercept,
		chasmRequestEngineInterceptor.Intercept,
		chasmRequestVisibilityInterceptor.Intercept,
	}
}

func RateLimitInterceptorProvider(
	serviceConfig *configs.Config,
) *interceptor.RateLimitInterceptor {
	return interceptor.NewRateLimitInterceptor(
		configs.NewPriorityRateLimiter(func() float64 { return float64(serviceConfig.RPS()) }, serviceConfig.OperatorRPSRatio),
		map[string]int{
			healthpb.Health_Check_FullMethodName:                         0, // exclude health check requests from rate limiting.
			historyservice.HistoryService_DeepHealthCheck_FullMethodName: 0, // exclude deep health check requests from rate limiting.
		},
	)
}

func ESProcessorConfigProvider(
	serviceConfig *configs.Config,
) *elasticsearch.ProcessorConfig {
	return &elasticsearch.ProcessorConfig{
		IndexerConcurrency:       serviceConfig.IndexerConcurrency,
		ESProcessorNumOfWorkers:  serviceConfig.ESProcessorNumOfWorkers,
		ESProcessorBulkActions:   serviceConfig.ESProcessorBulkActions,
		ESProcessorBulkSize:      serviceConfig.ESProcessorBulkSize,
		ESProcessorFlushInterval: serviceConfig.ESProcessorFlushInterval,
		ESProcessorAckTimeout:    serviceConfig.ESProcessorAckTimeout,
	}
}

func PersistenceRateLimitingParamsProvider(
	serviceConfig *configs.Config,
	persistenceLazyLoadedServiceResolver service.PersistenceLazyLoadedServiceResolver,
	ownershipBasedQuotaScaler shard.LazyLoadedOwnershipBasedQuotaScaler,
	logger log.SnTaggedLogger,
) service.PersistenceRateLimitingParams {
	hostCalculator := calculator.NewLoggedCalculator(
		shard.NewOwnershipAwareQuotaCalculator(
			ownershipBasedQuotaScaler,
			persistenceLazyLoadedServiceResolver,
			serviceConfig.PersistenceMaxQPS,
			serviceConfig.PersistenceGlobalMaxQPS,
		),
		log.With(logger, tag.ComponentPersistence, tag.ScopeHost),
	)
	namespaceCalculator := calculator.NewLoggedNamespaceCalculator(
		shard.NewOwnershipAwareNamespaceQuotaCalculator(
			ownershipBasedQuotaScaler,
			persistenceLazyLoadedServiceResolver,
			serviceConfig.PersistenceNamespaceMaxQPS,
			serviceConfig.PersistenceGlobalNamespaceMaxQPS,
		),
		log.With(logger, tag.ComponentPersistence, tag.ScopeNamespace),
	)
	return service.PersistenceRateLimitingParams{
		PersistenceMaxQps: func() int {
			return int(hostCalculator.GetQuota())
		},
		PersistenceNamespaceMaxQps: func(namespace string) int {
			return int(namespaceCalculator.GetQuota(namespace))
		},
		PersistencePerShardNamespaceMaxQPS: persistenceClient.PersistencePerShardNamespaceMaxQPS(serviceConfig.PersistencePerShardNamespaceMaxQPS),
		OperatorRPSRatio:                   persistenceClient.OperatorRPSRatio(serviceConfig.OperatorRPSRatio),
		PersistenceBurstRatio:              persistenceClient.PersistenceBurstRatio(serviceConfig.PersistenceQPSBurstRatio),
		DynamicRateLimitingParams:          persistenceClient.DynamicRateLimitingParams(serviceConfig.PersistenceDynamicRateLimitingParams),
	}
}

func VisibilityManagerProvider(
	logger log.Logger,
	metricsHandler metrics.Handler,
	persistenceConfig *config.Persistence,
	customVisibilityStoreFactory visibility.VisibilityStoreFactory,
	esProcessorConfig *elasticsearch.ProcessorConfig,
	serviceConfig *configs.Config,
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
		esProcessorConfig,
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
		serviceConfig.SecondaryVisibilityWritingMode,
		serviceConfig.VisibilityDisableOrderByClause,
		serviceConfig.VisibilityEnableManualPagination,
		serviceConfig.VisibilityEnableUnifiedQueryConverter,
		metricsHandler,
		logger,
		serializer,
	)
}

func ChasmVisibilityManagerProvider(
	chasmRegistry *chasm.Registry,
	visibilityManager manager.VisibilityManager,
) chasm.VisibilityManager {
	return visibility.NewChasmVisibilityManager(
		chasmRegistry,
		visibilityManager,
	)
}

func EventNotifierProvider(
	timeSource clock.TimeSource,
	metricsHandler metrics.Handler,
	config *configs.Config,
) events.Notifier {
	return events.NewNotifier(
		timeSource,
		metricsHandler,
		config.GetShardID,
	)
}

func ServiceLifetimeHooks(lc fx.Lifecycle, svc *Service) {
	lc.Append(fx.StartStopHook(svc.Start, svc.Stop))
}

func ReplicationProgressCacheProvider(
	serviceConfig *configs.Config,
	logger log.Logger,
	handler metrics.Handler,
) replication.ProgressCache {
	return replication.NewProgressCache(serviceConfig, logger, handler)
}

func VersionMembershipCacheProvider(
	lc fx.Lifecycle,
	serviceConfig *configs.Config,
	metricsHandler metrics.Handler,
) worker_versioning.VersionMembershipCache {
	c := commoncache.New(serviceConfig.VersionMembershipCacheMaxSize(), &commoncache.Options{
		TTL: max(1*time.Second, serviceConfig.VersionMembershipCacheTTL()),
	})
	lc.Append(fx.Hook{
		OnStop: func(context.Context) error {
			c.Stop()
			return nil
		},
	})
	return worker_versioning.NewVersionMembershipCache(c, metricsHandler)
}

func ReactivationSignalCacheProvider(
	lc fx.Lifecycle,
	serviceConfig *configs.Config,
	metricsHandler metrics.Handler,
) worker_versioning.ReactivationSignalCache {
	c := commoncache.New(serviceConfig.VersionReactivationSignalCacheMaxSize(), &commoncache.Options{
		TTL: max(1*time.Second, serviceConfig.VersionReactivationSignalCacheTTL()),
	})
	lc.Append(fx.Hook{
		OnStop: func(context.Context) error {
			c.Stop()
			return nil
		},
	})
	return worker_versioning.NewReactivationSignalCache(c, metricsHandler)
}

func RoutingInfoCacheProvider(
	lc fx.Lifecycle,
	serviceConfig *configs.Config,
	metricsHandler metrics.Handler,
) worker_versioning.RoutingInfoCache {
	c := commoncache.New(serviceConfig.RoutingInfoCacheMaxSize(), &commoncache.Options{
		TTL: max(1*time.Second, serviceConfig.RoutingInfoCacheTTL()),
	})
	lc.Append(fx.Hook{
		OnStop: func(context.Context) error {
			c.Stop()
			return nil
		},
	})
	return worker_versioning.NewRoutingInfoCache(c, metricsHandler)
}
