// Copyright © 2026 Hanzo AI. MIT License.

package tasks

// Wire types — JSON shapes shared by ZAP envelope + HTTP shim. Field
// names match the surface the embedded UI consumes today; staying close
// to the upstream Temporal Cloud API shapes keeps the SDK gain-of-port
// minimal, but every value here is owned by Hanzo (no proto, no fork).

type Namespace struct {
	NamespaceInfo NamespaceInfo `json:"namespaceInfo"`
	Config        NamespaceCfg  `json:"config"`
	IsActive      bool          `json:"isActive"`
}

type NamespaceInfo struct {
	Name        string `json:"name"`
	State       string `json:"state"` // NAMESPACE_STATE_REGISTERED|DEPRECATED|DELETED
	Description string `json:"description,omitempty"`
	OwnerEmail  string `json:"ownerEmail,omitempty"`
	Region      string `json:"region,omitempty"` // e.g. "do-sfo3", informational
	CreateTime  string `json:"createTime,omitempty"`
}

type NamespaceCfg struct {
	WorkflowExecutionRetentionTtl string `json:"workflowExecutionRetentionTtl"` // "720h"
	APSLimit                      int    `json:"apsLimit"`                      // actions per second
	HistoryArchivalState          string `json:"historyArchivalState,omitempty"`
	HistoryArchivalUri            string `json:"historyArchivalUri,omitempty"`
	VisibilityArchivalState       string `json:"visibilityArchivalState,omitempty"`
	VisibilityArchivalUri         string `json:"visibilityArchivalUri,omitempty"`
	CustomData                    map[string]string `json:"customData,omitempty"`
}

// SearchAttribute is a typed search attribute registered to a namespace.
type SearchAttribute struct {
	Name string `json:"name"`
	Type string `json:"type"` // Keyword|Text|Int|Double|Bool|Datetime|KeywordList
}

// WorkflowUserMetadata is operator-supplied summary/details for a workflow.
type WorkflowUserMetadata struct {
	Summary   string `json:"summary,omitempty"`
	Details   string `json:"details,omitempty"`
	UpdatedBy string `json:"updatedBy,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

type WorkflowExecution struct {
	Execution    ExecutionRef          `json:"execution"`
	Type         TypeRef               `json:"type"`
	StartTime    string                `json:"startTime,omitempty"`
	CloseTime    string                `json:"closeTime,omitempty"`
	Status       string                `json:"status"` // WORKFLOW_EXECUTION_STATUS_*
	TaskQueue    string                `json:"taskQueue,omitempty"`
	HistoryLen   int64                 `json:"historyLength,omitempty"`
	Memo         any                   `json:"memo,omitempty"`
	SearchAttrs  map[string]any        `json:"searchAttrs,omitempty"`
	Input        any                   `json:"input,omitempty"`
	Result       any                   `json:"result,omitempty"`
	UserMetadata *WorkflowUserMetadata `json:"userMetadata,omitempty"`
}

type ExecutionRef struct {
	WorkflowId string `json:"workflowId"`
	RunId      string `json:"runId"`
}

type TypeRef struct {
	Name string `json:"name"`
}

type Schedule struct {
	ScheduleId string         `json:"scheduleId"`
	Namespace  string         `json:"namespace"`
	Spec       ScheduleSpec   `json:"spec"`
	Action     ScheduleAction `json:"action"`
	State      ScheduleState  `json:"state"`
	Info       ScheduleInfo   `json:"info"`
}

type ScheduleSpec struct {
	CronString []string `json:"cronString,omitempty"`
	Interval   []struct {
		Interval string `json:"interval"`
		Phase    string `json:"phase,omitempty"`
	} `json:"interval,omitempty"`
}

type ScheduleAction struct {
	WorkflowType TypeRef `json:"workflowType"`
	TaskQueue    string  `json:"taskQueue"`
	Input        any     `json:"input,omitempty"`
}

type ScheduleState struct {
	Paused bool `json:"paused"`
	Note   string `json:"note,omitempty"`
}

type ScheduleInfo struct {
	CreateTime     string `json:"createTime"`
	UpdateTime     string `json:"updateTime,omitempty"`
	ActionCount    int64  `json:"actionCount"`
	NextActionTime string `json:"nextActionTime,omitempty"`
}

// BatchOperation — bulk terminate/cancel/signal across many executions.
type BatchOperation struct {
	BatchId    string `json:"batchId"`
	Namespace  string `json:"namespace"`
	Operation  string `json:"operation"` // BATCH_OPERATION_TYPE_TERMINATE|CANCEL|SIGNAL|RESET
	Reason     string `json:"reason"`
	Query      string `json:"query"` // visibility query — "WorkflowType='X'"
	State      string `json:"state"` // BATCH_OPERATION_STATE_RUNNING|COMPLETED|FAILED
	StartTime  string `json:"startTime"`
	CloseTime  string `json:"closeTime,omitempty"`
	TotalCount int64  `json:"totalOperationCount"`
	DoneCount  int64  `json:"completeOperationCount"`
}

// Deployment — a worker version series. Workers register a buildId; the
// engine routes workflow tasks based on default + ramping rules.
type Deployment struct {
	Name           string              `json:"name"`
	Namespace      string              `json:"namespace"`
	Description    string              `json:"description,omitempty"`
	OwnerEmail     string              `json:"ownerEmail,omitempty"`
	DefaultCompute string              `json:"defaultCompute,omitempty"`
	Versions       []DeploymentVersion `json:"versions"`
	DefaultBuildId string              `json:"defaultBuildId"`
	CreateTime     string              `json:"createTime"`
	UpdateTime     string              `json:"updateTime,omitempty"`
}

type DeploymentVersion struct {
	BuildId     string            `json:"buildId"`
	State       string            `json:"state"` // DEPLOYMENT_STATE_DRAFT|CURRENT|RAMPING|RETIRED
	Description string            `json:"description,omitempty"`
	Compute     string            `json:"compute,omitempty"`
	Image       string            `json:"image,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	CreateTime  string            `json:"createTime"`
	UpdateTime  string            `json:"updateTime,omitempty"`
}

// StandaloneActivity — a first-class activity record keyed by
// (ns, activityId, runId), independent of any workflow. Workers
// schedule, heartbeat, and complete activities that the engine tracks
// without an enclosing workflow execution.
type StandaloneActivity struct {
	Execution              ExecutionRef `json:"execution"`
	Type                   TypeRef      `json:"type"`
	TaskQueue              string       `json:"taskQueue,omitempty"`
	Status                 string       `json:"status"` // ACTIVITY_TASK_STATE_*
	StartTime              string       `json:"startTime,omitempty"`
	CloseTime              string       `json:"closeTime,omitempty"`
	RetryPolicy            *RetryPolicy `json:"retryPolicy,omitempty"`
	Input                  any          `json:"input,omitempty"`
	Result                 any          `json:"result,omitempty"`
	FailureCause           string       `json:"failureCause,omitempty"`
	Identity               string       `json:"identity,omitempty"`
	Attempt                int          `json:"attempt"`
	MaximumAttempts        int          `json:"maximumAttempts,omitempty"`
	ScheduleToCloseTimeout string       `json:"scheduleToCloseTimeout,omitempty"`
	ScheduleToStartTimeout string       `json:"scheduleToStartTimeout,omitempty"`
	StartToCloseTimeout    string       `json:"startToCloseTimeout,omitempty"`
	HeartbeatTimeout       string       `json:"heartbeatTimeout,omitempty"`
	LastHeartbeatTime      string       `json:"lastHeartbeatTime,omitempty"`
	HistoryLength          int64        `json:"historyLength,omitempty"`
}

// RetryPolicy — schedule retry knobs for an activity.
type RetryPolicy struct {
	InitialInterval        string   `json:"initialInterval,omitempty"`
	BackoffCoefficient     float64  `json:"backoffCoefficient,omitempty"`
	MaximumInterval        string   `json:"maximumInterval,omitempty"`
	MaximumAttempts        int      `json:"maximumAttempts,omitempty"`
	NonRetryableErrorTypes []string `json:"nonRetryableErrorTypes,omitempty"`
}

// NexusEndpoint — cross-namespace operation bridge.
type NexusEndpoint struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	Description string `json:"description,omitempty"`
	Target      string `json:"target"` // ns2://other-namespace/handler
	CreateTime  string `json:"createTime"`
}

// Identity — who is allowed to operate on this namespace. Sourced from
// IAM (X-User-Email) when wired, manually managed today.
type Identity struct {
	Email     string `json:"email"`
	Namespace string `json:"namespace"`
	Role      string `json:"role"` // owner|admin|developer|viewer
	GrantTime string `json:"grantTime"`
}

// HistoryEvent — a single durable record in a workflow execution's
// history. Modeled on Temporal's HistoryEvent but JSON-native and owned
// by Hanzo. EventId is monotonic per (namespace, workflowId, runId);
// EventType matches the WORKFLOW_EXECUTION_* / WORKFLOW_TASK_* /
// ACTIVITY_TASK_* / TIMER_* family.
type HistoryEvent struct {
	EventId    int64          `json:"eventId"`
	EventTime  string         `json:"eventTime"`
	EventType  string         `json:"eventType"`
	Attributes map[string]any `json:"attributes,omitempty"`
}
