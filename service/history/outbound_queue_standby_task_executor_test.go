package history

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	enumsspb "github.com/hanzoai/tasks/api/enums/v1"
	persistencespb "github.com/hanzoai/tasks/api/persistence/v1"
	"github.com/hanzoai/tasks/chasm"
	"github.com/hanzoai/tasks/common/cluster"
	"github.com/hanzoai/tasks/common/log"
	"github.com/hanzoai/tasks/common/metrics"
	"github.com/hanzoai/tasks/common/namespace"
	"github.com/hanzoai/tasks/common/testing/testvars"
	"github.com/hanzoai/tasks/service/history/hsm"
	historyi "github.com/hanzoai/tasks/service/history/interfaces"
	"github.com/hanzoai/tasks/service/history/queues"
	"github.com/hanzoai/tasks/service/history/shard"
	"github.com/hanzoai/tasks/service/history/tasks"
	"github.com/hanzoai/tasks/service/history/tests"
	"github.com/hanzoai/tasks/service/history/workflow/cache"
	"go.uber.org/mock/gomock"
)

type outboundQueueStandbyTaskExecutorSuite struct {
	suite.Suite
	*require.Assertions

	controller            *gomock.Controller
	mockShard             *shard.ContextTest
	mockWorkflowCache     *cache.MockCache
	mockChasmEngine       *chasm.MockEngine
	mockNamespaceRegistry *namespace.MockRegistry
	hsmRegistry           *hsm.Registry
	mockWorkflowContext   *historyi.MockWorkflowContext
	mockMutableState      *historyi.MockMutableState
	mockExecutable        *queues.MockExecutable
	mockChasmTree         *historyi.MockChasmTree

	logger         log.Logger
	metricsHandler metrics.Handler

	namespaceID    namespace.ID
	namespaceEntry *namespace.Namespace
	clusterName    string
	now            time.Time

	executor *outboundQueueStandbyTaskExecutor
}

func TestOutboundQueueStandbyTaskExecutorSuite(t *testing.T) {
	s := new(outboundQueueStandbyTaskExecutorSuite)
	suite.Run(t, s)
}

func (s *outboundQueueStandbyTaskExecutorSuite) SetupTest() {
	s.Assertions = require.New(s.T())

	s.namespaceID = tests.NamespaceID
	s.namespaceEntry = tests.GlobalNamespaceEntry
	s.clusterName = cluster.TestAlternativeClusterName
	s.now = time.Now()

	// Setup controller and mocks
	s.controller = gomock.NewController(s.T())

	s.mockShard = shard.NewTestContext(
		s.controller,
		&persistencespb.ShardInfo{
			ShardId: 1,
			RangeId: 1,
		},
		tests.NewDynamicConfig(),
	)
	s.mockWorkflowCache = cache.NewMockCache(s.controller)
	s.mockChasmEngine = chasm.NewMockEngine(s.controller)
	s.mockNamespaceRegistry = namespace.NewMockRegistry(s.controller)
	s.hsmRegistry = hsm.NewRegistry()
	s.mockWorkflowContext = historyi.NewMockWorkflowContext(s.controller)
	s.mockMutableState = historyi.NewMockMutableState(s.controller)
	s.mockExecutable = queues.NewMockExecutable(s.controller)
	s.mockChasmTree = historyi.NewMockChasmTree(s.controller)

	s.logger = s.mockShard.GetLogger()
	s.metricsHandler = s.mockShard.GetMetricsHandler()

	ns := namespace.NewLocalNamespaceForTest(&persistencespb.NamespaceInfo{
		Name: s.namespaceEntry.Name().String(),
		Id:   string(s.namespaceID),
	}, nil, "")
	s.mockNamespaceRegistry.EXPECT().GetNamespaceByID(gomock.Any()).Return(ns, nil).AnyTimes()
	s.mockNamespaceRegistry.EXPECT().GetNamespaceName(gomock.Any()).Return(ns.Name(), nil).AnyTimes()

	s.mockShard.Resource.ClusterMetadata.EXPECT().GetClusterID().Return(int64(s.mockShard.GetShardID())).AnyTimes()
	s.mockShard.Resource.NamespaceCache.EXPECT().GetNamespaceByID(gomock.Any()).Return(s.namespaceEntry, nil).AnyTimes()
	s.mockShard.SetStateMachineRegistry(s.hsmRegistry)
	s.mockShard.Resource.NamespaceCache.EXPECT().
		GetNamespaceByID(gomock.Any()).
		Return(s.namespaceEntry, nil).
		AnyTimes()

	s.mockWorkflowCache.EXPECT().GetOrCreateCurrentExecution(
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
	).Return(cache.NoopReleaseFn, nil).AnyTimes()

	s.mockMutableState.EXPECT().GetCurrentVersion().Return(int64(1)).AnyTimes()
	s.mockMutableState.EXPECT().NextTransitionCount().Return(int64(0)).AnyTimes()
	s.mockMutableState.EXPECT().GetWorkflowKey().Return(tests.WorkflowKey).AnyTimes()
	s.mockMutableState.EXPECT().GetExecutionState().Return(&persistencespb.WorkflowExecutionState{
		State: enumsspb.WORKFLOW_EXECUTION_STATE_RUNNING,
	}).AnyTimes()

	s.executor = newOutboundQueueStandbyTaskExecutor(
		s.mockShard,
		s.mockWorkflowCache,
		s.clusterName,
		s.logger,
		s.metricsHandler,
		s.mockChasmEngine,
	)
}

func (s *outboundQueueStandbyTaskExecutorSuite) TearDownTest() {
	s.controller.Finish()
	s.mockShard.StopForTest()
}

func (s *outboundQueueStandbyTaskExecutorSuite) TestExecute_ChasmTask() {
	tv := testvars.New(s.T())
	ctx := context.Background()

	testCases := []struct {
		name                string
		setupMocks          func(*tasks.ChasmTask)
		expectHandlerCalled bool
		expectedError       string
	}{
		{
			name: "success",
			setupMocks: func(task *tasks.ChasmTask) {
				// Setup successful workflow context loading and CHASM execution

				s.mockWorkflowCache.EXPECT().
					GetOrCreateChasmExecution(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), tests.ArchetypeID, gomock.Any()).
					Return(s.mockWorkflowContext, func(error) {}, nil)

				s.mockWorkflowContext.EXPECT().
					LoadMutableState(gomock.Any(), gomock.Any()).
					Return(s.mockMutableState, nil)

				s.mockMutableState.EXPECT().
					ChasmTree().
					Return(s.mockChasmTree)

				s.mockChasmTree.EXPECT().
					ValidateSideEffectTask(
						gomock.Any(),
						gomock.Any(),
					)
			},
			expectHandlerCalled: true,
		},
		{
			name: "mutable state failure",
			setupMocks: func(task *tasks.ChasmTask) {
				// Workflow context loads but mutable state fails
				s.mockWorkflowCache.EXPECT().
					GetOrCreateChasmExecution(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), tests.ArchetypeID, gomock.Any()).
					Return(s.mockWorkflowContext, func(error) {}, nil)

				s.mockWorkflowContext.EXPECT().
					LoadMutableState(gomock.Any(), gomock.Any()).
					Return(nil, errors.New("mutable state failed to load"))
			},
			expectHandlerCalled: false,
			expectedError:       "mutable state failed to load",
		},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			// Create a CHASM task
			task := &tasks.ChasmTask{
				WorkflowKey: tv.Any().WorkflowKey(),
				TaskID:      s.mustGenerateTaskID(),
				Category:    tasks.CategoryOutbound,
				Destination: tv.Any().String(),
				Info: &persistencespb.ChasmTaskInfo{
					TypeId:      tv.Any().UInt32(),
					ArchetypeId: tests.ArchetypeID,
				},
				VisibilityTimestamp: s.now,
			}

			tc.setupMocks(task)
			s.mockExecutable.EXPECT().GetTask().Return(task).AnyTimes()

			result := s.executor.Execute(ctx, s.mockExecutable)

			if tc.expectedError != "" {
				s.Error(result.ExecutionErr)
				s.Contains(result.ExecutionErr.Error(), tc.expectedError)
			} else {
				s.NoError(result.ExecutionErr)
			}
			s.False(result.ExecutedAsActive)
			s.NotEmpty(result.ExecutionMetricTags)
		})
	}
}

func (s *outboundQueueStandbyTaskExecutorSuite) TestExecute_PreValidationFails() {
	tv := testvars.New(s.T())
	ctx := context.Background()

	testCases := []struct {
		name          string
		setupTask     func() tasks.Task
		setupMocks    func(tasks.Task)
		expectedError string
	}{
		{
			name: "invalid task type",
			setupTask: func() tasks.Task {
				// Create a task type that's NOT StateMachineOutboundTask or ChasmTask
				return &tasks.ActivityTask{
					WorkflowKey:         tv.Any().WorkflowKey(),
					TaskID:              s.mustGenerateTaskID(),
					VisibilityTimestamp: s.now,
					TaskQueue:           tv.Any().String(),
				}
			},
			setupMocks:    func(task tasks.Task) {},
			expectedError: "unknown task type",
		},
		{
			name: "clock validation failure",
			setupTask: func() tasks.Task {
				return &tasks.ChasmTask{
					Destination:         tv.Any().String(),
					TaskID:              math.MaxInt64,
					Info:                &persistencespb.ChasmTaskInfo{},
					VisibilityTimestamp: s.now,
					Category:            tasks.Category{},
				}
			},
			setupMocks:    func(task tasks.Task) {},
			expectedError: "task clock validation failed",
		},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			task := tc.setupTask()
			tc.setupMocks(task)
			s.mockExecutable.EXPECT().GetTask().Return(task)

			result := s.executor.Execute(ctx, s.mockExecutable)

			s.Error(result.ExecutionErr)
			s.Contains(result.ExecutionErr.Error(), tc.expectedError)
			s.False(result.ExecutedAsActive)
			s.NotEmpty(result.ExecutionMetricTags)
		})
	}
}

func (s *outboundQueueStandbyTaskExecutorSuite) mustGenerateTaskID() int64 {
	taskID, err := s.mockShard.GenerateTaskID()
	s.NoError(err)
	return taskID
}
