# Hanzo Tasks — native ZAP schema for the canonical workflow RPC
# surface. This is the one and only wire contract. Nothing on this
# wire speaks protobuf, gRPC, or any temporal.io-branded framing.
#
# Opcodes are stable across releases. Adding a new RPC appends a
# new opcode; never reuse. Request/response structs are evolved
# only by adding optional fields (future — tag-based at serde).
#
# Layout mirrors the service boundaries in hanzoai/tasks:
#   frontend:  user/SDK-facing workflow + schedule ops
#   history:   execution engine (internal)
#   matching:  task queue + workflow-task dispatch (internal)
#   worker:    scheduler + system workflows (internal)

# ── Core value types ───────────────────────────────────────────

struct WorkflowExecution
  workflowId Text
  runId Text

struct WorkflowType
  name Text

struct TaskQueue
  name Text
  kind Int8   # 0=normal, 1=sticky

struct Payload
  metadata Bytes   # MIME-typed: application/json, application/zap, etc.
  data Bytes

struct Payloads
  items List(Payload)

struct RetryPolicy
  initialIntervalMs Int64
  backoffCoefficient Float64
  maximumIntervalMs Int64
  maximumAttempts Int32
  nonRetryableErrorTypes List(Text)

struct Timeouts
  workflowExecutionMs Int64
  workflowRunMs Int64
  workflowTaskMs Int64

# ── Namespace ops ──────────────────────────────────────────────

struct NamespaceInfo
  name Text
  state Int8       # 0=unspecified, 1=registered, 2=deprecated, 3=deleted
  description Text
  ownerEmail Text
  id Text

struct NamespaceConfig
  retentionMs Int64

struct Namespace
  info NamespaceInfo
  config NamespaceConfig

# ── Workflow execution ops ─────────────────────────────────────

struct WorkflowExecutionInfo
  execution WorkflowExecution
  type WorkflowType
  startTimeMs Int64
  closeTimeMs Int64
  status Int8     # 0=unspecified, 1=running, 2=completed,
                  # 3=failed, 4=canceled, 5=terminated,
                  # 6=continued_as_new, 7=timed_out
  historyLength Int64
  taskQueue Text
  memo Payloads

struct StartWorkflowRequest
  namespace Text
  workflowId Text
  workflowType WorkflowType
  taskQueue TaskQueue
  input Payloads
  retryPolicy RetryPolicy
  timeouts Timeouts
  memo Payloads

struct StartWorkflowResponse
  runId Text

struct SignalWorkflowRequest
  namespace Text
  execution WorkflowExecution
  signalName Text
  input Payloads

struct CancelWorkflowRequest
  namespace Text
  execution WorkflowExecution
  reason Text

struct TerminateWorkflowRequest
  namespace Text
  execution WorkflowExecution
  reason Text

struct DescribeWorkflowRequest
  namespace Text
  execution WorkflowExecution

struct DescribeWorkflowResponse
  info WorkflowExecutionInfo

struct ListWorkflowsRequest
  namespace Text
  query Text        # SQL-subset filter ("WorkflowType='X' AND Status='Running'")
  pageSize Int32
  nextPageToken Bytes

struct ListWorkflowsResponse
  executions List(WorkflowExecutionInfo)
  nextPageToken Bytes

# ── Schedule ops ───────────────────────────────────────────────

struct ScheduleSpec
  cron List(Text)          # zero or more cron expressions
  intervalMs Int64         # or single interval
  startTimeMs Int64
  endTimeMs Int64
  jitterMs Int64
  timezone Text

struct ScheduleAction
  workflowId Text
  workflowType WorkflowType
  taskQueue TaskQueue
  input Payloads

struct Schedule
  id Text
  spec ScheduleSpec
  action ScheduleAction
  paused Bool

struct CreateScheduleRequest
  namespace Text
  scheduleId Text
  schedule Schedule

struct ListSchedulesRequest
  namespace Text
  pageSize Int32
  nextPageToken Bytes

struct ListSchedulesResponse
  schedules List(Schedule)
  nextPageToken Bytes

# ── Task queue ops (internal: matching service) ────────────────

struct PollWorkflowTaskRequest
  namespace Text
  taskQueue TaskQueue
  identity Text
  workerBuildId Text

struct WorkflowTask
  taskToken Bytes
  workflowExecution WorkflowExecution
  workflowType WorkflowType
  history Bytes              # encoded WorkflowHistory frame
  nextPageToken Bytes

struct PollActivityTaskRequest
  namespace Text
  taskQueue TaskQueue
  identity Text

struct ActivityTask
  taskToken Bytes
  workflowExecution WorkflowExecution
  activityId Text
  activityType Text
  input Payloads
  scheduledTimeMs Int64
  startToCloseTimeoutMs Int64
  heartbeatTimeoutMs Int64

struct RespondWorkflowTaskCompletedRequest
  taskToken Bytes
  commands Bytes             # encoded command list

struct RespondActivityTaskCompletedRequest
  taskToken Bytes
  result Payloads

struct RespondActivityTaskFailedRequest
  taskToken Bytes
  failure Bytes              # encoded failure

struct RecordActivityTaskHeartbeatRequest
  taskToken Bytes
  details Payloads

struct RecordActivityTaskHeartbeatResponse
  cancelRequested Bool

# ── Canonical RPC service ──────────────────────────────────────

interface Tasks
  # Namespace
  listNamespaces (pageSize Int32, nextPageToken Bytes)
    -> (namespaces List(Namespace), nextPageToken Bytes)
  describeNamespace (name Text)
    -> (namespace Namespace)
  registerNamespace (namespace Namespace)
    -> (ok Bool)

  # Workflow lifecycle
  startWorkflow (req StartWorkflowRequest)
    -> (resp StartWorkflowResponse)
  signalWorkflow (req SignalWorkflowRequest)
    -> (ok Bool)
  cancelWorkflow (req CancelWorkflowRequest)
    -> (ok Bool)
  terminateWorkflow (req TerminateWorkflowRequest)
    -> (ok Bool)
  describeWorkflow (req DescribeWorkflowRequest)
    -> (resp DescribeWorkflowResponse)
  listWorkflows (req ListWorkflowsRequest)
    -> (resp ListWorkflowsResponse)

  # Schedules
  createSchedule (req CreateScheduleRequest)
    -> (ok Bool)
  listSchedules (req ListSchedulesRequest)
    -> (resp ListSchedulesResponse)
  deleteSchedule (namespace Text, scheduleId Text)
    -> (ok Bool)
  pauseSchedule (namespace Text, scheduleId Text, paused Bool)
    -> (ok Bool)

  # Task queue (matching)
  pollWorkflowTask (req PollWorkflowTaskRequest)
    -> (task WorkflowTask)
  pollActivityTask (req PollActivityTaskRequest)
    -> (task ActivityTask)
  respondWorkflowTaskCompleted (req RespondWorkflowTaskCompletedRequest)
    -> (ok Bool)
  respondActivityTaskCompleted (req RespondActivityTaskCompletedRequest)
    -> (ok Bool)
  respondActivityTaskFailed (req RespondActivityTaskFailedRequest)
    -> (ok Bool)
  recordActivityTaskHeartbeat (req RecordActivityTaskHeartbeatRequest)
    -> (resp RecordActivityTaskHeartbeatResponse)

  # Health
  health () -> (service Text, status Text)
