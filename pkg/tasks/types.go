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
}

type WorkflowExecution struct {
	Execution   ExecutionRef `json:"execution"`
	Type        TypeRef      `json:"type"`
	StartTime   string       `json:"startTime,omitempty"`
	CloseTime   string       `json:"closeTime,omitempty"`
	Status      string       `json:"status"` // WORKFLOW_EXECUTION_STATUS_*
	TaskQueue   string       `json:"taskQueue,omitempty"`
	HistoryLen  int64        `json:"historyLength,omitempty"`
	Memo        any          `json:"memo,omitempty"`
	SearchAttrs map[string]any `json:"searchAttrs,omitempty"`
	Input       any          `json:"input,omitempty"`
	Result      any          `json:"result,omitempty"`
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
	SeriesName  string             `json:"seriesName"`
	Namespace   string             `json:"namespace"`
	BuildIDs    []DeploymentBuild  `json:"buildIds"`
	DefaultBuildId string          `json:"defaultBuildId"`
	CreateTime  string             `json:"createTime"`
}

type DeploymentBuild struct {
	BuildId    string `json:"buildId"`
	State      string `json:"state"` // DEPLOYMENT_STATE_DRAFT|CURRENT|RAMPING|RETIRED
	CreateTime string `json:"createTime"`
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
