package addsearchattributes

import (
	"context"

	sdkworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"github.com/hanzoai/tasks/common/headers"
	"github.com/hanzoai/tasks/common/log"
	"github.com/hanzoai/tasks/common/metrics"
	esclient "github.com/hanzoai/tasks/common/persistence/visibility/store/elasticsearch/client"
	"github.com/hanzoai/tasks/common/primitives"
	"github.com/hanzoai/tasks/common/searchattribute"
	workercommon "github.com/hanzoai/tasks/service/worker/common"
	"go.uber.org/fx"
)

type (
	// addSearchAttributes represent background work needed for adding search attributes
	addSearchAttributes struct {
		initParams
	}

	initParams struct {
		fx.In
		EsClient       esclient.Client
		Manager        searchattribute.Manager
		MetricsHandler metrics.Handler
		Logger         log.Logger
	}
)

var Module = workercommon.AnnotateWorkerComponentProvider(newComponent)

func newComponent(params initParams) workercommon.WorkerComponent {
	return &addSearchAttributes{initParams: params}
}

func (wc *addSearchAttributes) RegisterWorkflow(registry sdkworker.Registry) {
	registry.RegisterWorkflowWithOptions(AddSearchAttributesWorkflow, workflow.RegisterOptions{Name: WorkflowName})
}

func (wc *addSearchAttributes) DedicatedWorkflowWorkerOptions() *workercommon.DedicatedWorkerOptions {
	// use default worker
	return nil
}

func (wc *addSearchAttributes) RegisterActivities(registry sdkworker.Registry) {
	registry.RegisterActivity(wc.activities())
}

func (wc *addSearchAttributes) DedicatedActivityWorkerOptions() *workercommon.DedicatedWorkerOptions {
	return &workercommon.DedicatedWorkerOptions{
		TaskQueue: primitives.AddSearchAttributesActivityTQ,
		Options: sdkworker.Options{
			BackgroundActivityContext: headers.SetCallerType(context.Background(), headers.CallerTypeAPI),
		},
	}
}

func (wc *addSearchAttributes) activities() *activities {
	return &activities{
		esClient:       wc.EsClient,
		saManager:      wc.Manager,
		metricsHandler: wc.MetricsHandler.WithTags(metrics.OperationTag(metrics.AddSearchAttributesWorkflowScope)),
		logger:         wc.Logger,
	}
}
