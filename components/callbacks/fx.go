package callbacks

import (
	"fmt"
	"net/http"

	"github.com/hanzoai/tasks/common"
	"github.com/hanzoai/tasks/common/cluster"
	"github.com/hanzoai/tasks/common/collection"
	"github.com/hanzoai/tasks/common/log"
	"github.com/hanzoai/tasks/common/namespace"
	commonnexus "github.com/hanzoai/tasks/common/nexus"
	queuescommon "github.com/hanzoai/tasks/service/history/queues/common"
	"go.uber.org/fx"
)

var Module = fx.Module(
	"component.callbacks",
	fx.Provide(ConfigProvider),
	fx.Provide(HTTPCallerProviderProvider),
	fx.Invoke(RegisterTaskSerializers),
	fx.Invoke(RegisterStateMachine),
	fx.Invoke(RegisterExecutor),
)

func HTTPCallerProviderProvider(
	clusterMetadata cluster.Metadata,
	namespaceRegistry namespace.Registry,
	rpcFactory common.RPCFactory,
	httpClientCache *cluster.FrontendHTTPClientCache,
	logger log.Logger,
) (HTTPCallerProvider, error) {
	localClient, err := rpcFactory.CreateLocalFrontendHTTPClient()
	if err != nil {
		return nil, fmt.Errorf("cannot create local frontend HTTP client: %w", err)
	}
	defaultClient := &http.Client{}
	callbackTokenGenerator := commonnexus.NewCallbackTokenGenerator()

	m := collection.NewOnceMap(func(queuescommon.NamespaceIDAndDestination) HTTPCaller {
		return func(r *http.Request) (*http.Response, error) {
			return routeRequest(r,
				clusterMetadata,
				namespaceRegistry,
				httpClientCache,
				callbackTokenGenerator,
				defaultClient,
				localClient,
				logger,
			)
		}
	})
	return m.Get, nil
}
