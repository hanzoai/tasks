package deletemanager

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/api/serviceerror"
	persistencespb "github.com/hanzoai/tasks/api/persistence/v1"
	"github.com/hanzoai/tasks/chasm"
	"github.com/hanzoai/tasks/common/clock"
	"github.com/hanzoai/tasks/common/cluster"
	"github.com/hanzoai/tasks/common/definition"
	"github.com/hanzoai/tasks/common/metrics"
	"github.com/hanzoai/tasks/common/namespace"
	"github.com/hanzoai/tasks/common/persistence/visibility/manager"
	historyi "github.com/hanzoai/tasks/service/history/interfaces"
	"github.com/hanzoai/tasks/service/history/tasks"
	"github.com/hanzoai/tasks/service/history/tests"
	"github.com/hanzoai/tasks/service/history/workflow"
	wcache "github.com/hanzoai/tasks/service/history/workflow/cache"
	"go.uber.org/mock/gomock"
)

type (
	deleteManagerWorkflowSuite struct {
		suite.Suite
		*require.Assertions

		controller            *gomock.Controller
		mockCache             *wcache.MockCache
		mockShardContext      *historyi.MockShardContext
		mockClock             *clock.EventTimeSource
		mockNamespaceRegistry *namespace.MockRegistry
		mockMetadata          *cluster.MockMetadata
		mockVisibilityManager *manager.MockVisibilityManager

		deleteManager DeleteManager
	}
)

func TestDeleteManagerSuite(t *testing.T) {
	s := &deleteManagerWorkflowSuite{}
	suite.Run(t, s)
}

func (s *deleteManagerWorkflowSuite) SetupSuite() {

}

func (s *deleteManagerWorkflowSuite) TearDownSuite() {

}

func (s *deleteManagerWorkflowSuite) SetupTest() {
	s.Assertions = require.New(s.T())

	s.controller = gomock.NewController(s.T())
	s.mockCache = wcache.NewMockCache(s.controller)
	s.mockClock = clock.NewEventTimeSource()
	s.mockNamespaceRegistry = namespace.NewMockRegistry(s.controller)
	s.mockMetadata = cluster.NewMockMetadata(s.controller)
	s.mockVisibilityManager = manager.NewMockVisibilityManager(s.controller)
	s.mockVisibilityManager.EXPECT().GetIndexName().Return("").AnyTimes()

	config := tests.NewDynamicConfig()
	s.mockShardContext = historyi.NewMockShardContext(s.controller)
	s.mockShardContext.EXPECT().GetMetricsHandler().Return(metrics.NoopMetricsHandler).AnyTimes()
	s.mockShardContext.EXPECT().GetNamespaceRegistry().Return(s.mockNamespaceRegistry).AnyTimes()
	s.mockShardContext.EXPECT().GetClusterMetadata().Return(s.mockMetadata).AnyTimes()

	s.deleteManager = NewDeleteManager(
		s.mockShardContext,
		s.mockCache,
		config,
		s.mockClock,
		s.mockVisibilityManager,
	)
}

func (s *deleteManagerWorkflowSuite) TestDeleteDeletedWorkflowExecution() {
	we := commonpb.WorkflowExecution{
		WorkflowId: tests.WorkflowID,
		RunId:      tests.RunID,
	}

	mockWeCtx := historyi.NewMockWorkflowContext(s.controller)
	mockMutableState := historyi.NewMockMutableState(s.controller)
	mockMutableState.EXPECT().GetCurrentBranchToken().Return([]byte{22, 8, 78}, nil)
	closeExecutionVisibilityTaskID := int64(39)
	mockMutableState.EXPECT().GetExecutionInfo().Return(&persistencespb.WorkflowExecutionInfo{
		CloseVisibilityTaskId: closeExecutionVisibilityTaskID,
	})
	mockMutableState.EXPECT().ChasmTree().Return(workflow.NoopChasmTree).AnyTimes()
	stage := tasks.DeleteWorkflowExecutionStageNone

	s.mockShardContext.EXPECT().DeleteWorkflowExecution(
		gomock.Any(),
		definition.WorkflowKey{
			NamespaceID: tests.NamespaceID.String(),
			WorkflowID:  tests.WorkflowID,
			RunID:       tests.RunID,
		},
		chasm.WorkflowArchetypeID,
		[]byte{22, 8, 78},
		closeExecutionVisibilityTaskID,
		time.Unix(0, 0).UTC(),
		&stage,
	).Return(nil)
	mockWeCtx.EXPECT().Clear()

	err := s.deleteManager.DeleteWorkflowExecution(
		context.Background(),
		tests.NamespaceID,
		&we,
		mockWeCtx,
		mockMutableState,
		&stage,
	)
	s.NoError(err)
}

func (s *deleteManagerWorkflowSuite) TestDeleteDeletedWorkflowExecution_Error() {
	we := commonpb.WorkflowExecution{
		WorkflowId: tests.WorkflowID,
		RunId:      tests.RunID,
	}

	mockWeCtx := historyi.NewMockWorkflowContext(s.controller)
	mockMutableState := historyi.NewMockMutableState(s.controller)
	mockMutableState.EXPECT().GetCurrentBranchToken().Return([]byte{22, 8, 78}, nil)
	closeExecutionVisibilityTaskID := int64(39)
	mockMutableState.EXPECT().GetExecutionInfo().MinTimes(1).Return(&persistencespb.WorkflowExecutionInfo{
		CloseVisibilityTaskId: closeExecutionVisibilityTaskID,
	})
	mockMutableState.EXPECT().ChasmTree().Return(workflow.NoopChasmTree).AnyTimes()
	stage := tasks.DeleteWorkflowExecutionStageNone

	s.mockShardContext.EXPECT().DeleteWorkflowExecution(
		gomock.Any(),
		definition.WorkflowKey{
			NamespaceID: tests.NamespaceID.String(),
			WorkflowID:  tests.WorkflowID,
			RunID:       tests.RunID,
		},
		chasm.WorkflowArchetypeID,
		[]byte{22, 8, 78},
		closeExecutionVisibilityTaskID,
		time.Unix(0, 0).UTC(),
		&stage,
	).Return(serviceerror.NewInternal("test error"))

	err := s.deleteManager.DeleteWorkflowExecution(
		context.Background(),
		tests.NamespaceID,
		&we,
		mockWeCtx,
		mockMutableState,
		&stage,
	)
	s.Error(err)
}

func (s *deleteManagerWorkflowSuite) TestDeleteWorkflowExecution_OpenWorkflow() {
	we := commonpb.WorkflowExecution{
		WorkflowId: tests.WorkflowID,
		RunId:      tests.RunID,
	}

	mockWeCtx := historyi.NewMockWorkflowContext(s.controller)
	mockMutableState := historyi.NewMockMutableState(s.controller)
	closeExecutionVisibilityTaskID := int64(39)
	mockMutableState.EXPECT().GetCurrentBranchToken().Return([]byte{22, 8, 78}, nil)
	mockMutableState.EXPECT().GetExecutionInfo().MinTimes(1).Return(&persistencespb.WorkflowExecutionInfo{
		CloseVisibilityTaskId: closeExecutionVisibilityTaskID,
	})
	mockMutableState.EXPECT().ChasmTree().Return(workflow.NoopChasmTree).AnyTimes()
	stage := tasks.DeleteWorkflowExecutionStageNone

	s.mockShardContext.EXPECT().DeleteWorkflowExecution(
		gomock.Any(),
		definition.WorkflowKey{
			NamespaceID: tests.NamespaceID.String(),
			WorkflowID:  tests.WorkflowID,
			RunID:       tests.RunID,
		},
		chasm.WorkflowArchetypeID,
		[]byte{22, 8, 78},
		closeExecutionVisibilityTaskID,
		time.Unix(0, 0).UTC(),
		&stage,
	).Return(nil)
	mockWeCtx.EXPECT().Clear()

	err := s.deleteManager.DeleteWorkflowExecution(
		context.Background(),
		tests.NamespaceID,
		&we,
		mockWeCtx,
		mockMutableState,
		&stage,
	)
	s.NoError(err)
}
