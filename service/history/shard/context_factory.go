package shard

import (
	"github.com/hanzoai/tasks/chasm"
	"github.com/hanzoai/tasks/client"
	"github.com/hanzoai/tasks/common/archiver"
	"github.com/hanzoai/tasks/common/clock"
	"github.com/hanzoai/tasks/common/cluster"
	"github.com/hanzoai/tasks/common/config"
	"github.com/hanzoai/tasks/common/log"
	"github.com/hanzoai/tasks/common/membership"
	"github.com/hanzoai/tasks/common/metrics"
	"github.com/hanzoai/tasks/common/namespace"
	"github.com/hanzoai/tasks/common/persistence"
	"github.com/hanzoai/tasks/common/persistence/serialization"
	"github.com/hanzoai/tasks/common/resource"
	"github.com/hanzoai/tasks/common/searchattribute"
	"github.com/hanzoai/tasks/service/history/configs"
	"github.com/hanzoai/tasks/service/history/events"
	"github.com/hanzoai/tasks/service/history/hsm"
	historyi "github.com/hanzoai/tasks/service/history/interfaces"
	"github.com/hanzoai/tasks/service/history/tasks"
	"go.uber.org/fx"
)

//go:generate mockgen -package $GOPACKAGE -source $GOFILE -destination context_factory_mock.go

type (
	CloseCallback func(historyi.ControllableContext)

	ContextFactory interface {
		CreateContext(shardID int32, closeCallback CloseCallback) (historyi.ControllableContext, error)
	}

	ContextFactoryParams struct {
		fx.In

		ArchivalMetadata            archiver.ArchivalMetadata
		ClientBean                  client.Bean
		ClusterMetadata             cluster.Metadata
		Config                      *configs.Config
		PersistenceConfig           config.Persistence
		EngineFactory               EngineFactory
		HistoryClient               resource.HistoryClient
		HistoryServiceResolver      membership.ServiceResolver
		HostInfoProvider            membership.HostInfoProvider
		Logger                      log.Logger
		MetricsHandler              metrics.Handler
		NamespaceRegistry           namespace.Registry
		PayloadSerializer           serialization.Serializer
		PersistenceExecutionManager persistence.ExecutionManager
		PersistenceShardManager     persistence.ShardManager
		SaMapperProvider            searchattribute.MapperProvider
		SaProvider                  searchattribute.Provider
		ThrottledLogger             log.ThrottledLogger
		TimeSource                  clock.TimeSource
		TaskCategoryRegistry        tasks.TaskCategoryRegistry
		EventsCache                 events.Cache

		StateMachineRegistry *hsm.Registry
		ChasmRegistry        *chasm.Registry
	}

	contextFactoryImpl struct {
		*ContextFactoryParams
	}
)

func ContextFactoryProvider(params ContextFactoryParams) ContextFactory {
	return &contextFactoryImpl{
		ContextFactoryParams: &params,
	}
}

func (c *contextFactoryImpl) CreateContext(
	shardID int32,
	closeCallback CloseCallback,
) (historyi.ControllableContext, error) {
	shard, err := newContext(
		shardID,
		c.EngineFactory,
		c.Config,
		c.PersistenceConfig,
		closeCallback,
		c.Logger,
		c.ThrottledLogger,
		c.PersistenceExecutionManager,
		c.PersistenceShardManager,
		c.ClientBean,
		c.HistoryClient,
		c.MetricsHandler,
		c.PayloadSerializer,
		c.TimeSource,
		c.NamespaceRegistry,
		c.SaProvider,
		c.SaMapperProvider,
		c.ClusterMetadata,
		c.ArchivalMetadata,
		c.HostInfoProvider,
		c.TaskCategoryRegistry,
		c.EventsCache,
		c.StateMachineRegistry,
		c.ChasmRegistry,
	)
	if err != nil {
		return nil, err
	}
	shard.start()
	return shard, nil
}
