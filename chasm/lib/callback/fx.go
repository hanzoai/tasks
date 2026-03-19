package callback

import (
	"fmt"
	"net/http"

	"github.com/hanzoai/tasks/chasm"
	"github.com/hanzoai/tasks/common"
	"github.com/hanzoai/tasks/common/cluster"
	"github.com/hanzoai/tasks/common/collection"
	"github.com/hanzoai/tasks/common/log"
	"github.com/hanzoai/tasks/common/namespace"
	commonnexus "github.com/hanzoai/tasks/common/nexus"
	queuescommon "github.com/hanzoai/tasks/service/history/queues/common"
	"go.uber.org/fx"
)

func register(
	registry *chasm.Registry,
	library *Library,
) error {
	return registry.Register(library)
}

// httpCallerProviderProvider provides an HTTPCallerProvider for CHASM callbacks.
func httpCallerProviderProvider(
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

var Module = fx.Module(
	"chasm.lib.callback",
	fx.Provide(configProvider),
	fx.Provide(httpCallerProviderProvider),
	fx.Provide(NewInvocationTaskExecutor),
	fx.Provide(NewBackoffTaskExecutor),
	fx.Provide(newLibrary),
	fx.Invoke(register),
)
