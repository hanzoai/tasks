package workflow

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"
	"github.com/hanzoai/tasks/common/dynamicconfig"
	"github.com/hanzoai/tasks/common/log"
	"github.com/hanzoai/tasks/common/metrics"
	"github.com/hanzoai/tasks/common/metrics/metricstest"
	"github.com/hanzoai/tasks/common/namespace"
	"github.com/hanzoai/tasks/service/history/configs"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestEmitWorkflowCompletionStats_WorkflowDuration(t *testing.T) {
	logger := log.NewTestLogger()
	testHandler, _ := metricstest.NewHandler(logger, metrics.ClientConfig{})
	testNamespace := namespace.Name("test-namespace")
	config := &configs.Config{
		BreakdownMetricsByTaskQueue: dynamicconfig.GetBoolPropertyFnFilteredByTaskQueue(true),
	}

	completionMetric := completionMetric{
		initialized:      true,
		isWorkflow:       true,
		taskQueue:        "test-task-queue",
		namespaceState:   "active",
		workflowTypeName: "test-workflow",
		status:           enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED,
		startTime:        timestamppb.New(time.Unix(100, 0)),
		closeTime:        timestamppb.New(time.Unix(130, 0)),
	}

	emitWorkflowCompletionStats(testHandler, testNamespace, completionMetric, config)

	snapshot, err := testHandler.Snapshot()
	require.NoError(t, err)
	buckets, err := snapshot.Histogram("workflow_schedule_to_close_latency_milliseconds",

		metrics.StringTag("namespace", "test-namespace"),
		metrics.StringTag("namespace_state", "active"),
		metrics.StringTag("workflowType", "test-workflow"),
		metrics.StringTag("operation", "CompletionStats"),
		metrics.StringTag("taskqueue", "test-task-queue"),
		metrics.StringTag("otel_scope_name", "temporal"),
		metrics.StringTag("otel_scope_version", ""),
	)
	require.NoError(t, err)
	require.NotEmpty(t, buckets)
}

func TestEmitWorkflowCompletionStats_SkipNonWorkflow(t *testing.T) {
	logger := log.NewTestLogger()
	testHandler, _ := metricstest.NewHandler(logger, metrics.ClientConfig{})
	testNamespace := namespace.Name("test-namespace")
	completionMetric := completionMetric{isWorkflow: false}
	emitWorkflowCompletionStats(testHandler, testNamespace, completionMetric, nil)
	snapshot, err := testHandler.Snapshot()
	require.NoError(t, err)
	_, err = snapshot.Histogram("workflow_schedule_to_close_latency_milliseconds")
	require.Error(t, err)
}
