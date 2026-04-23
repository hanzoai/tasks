package client

import (
	"context"
	"errors"
	"time"
)

// Namespace mirrors schema/tasks.zap Namespace for the v1 JSON wire.
type Namespace struct {
	Info   NamespaceInfo   `json:"info"`
	Config NamespaceConfig `json:"config"`
}

// NamespaceInfo mirrors schema/tasks.zap NamespaceInfo.
type NamespaceInfo struct {
	Name        string         `json:"name"`
	State       NamespaceState `json:"state"`
	Description string         `json:"description,omitempty"`
	OwnerEmail  string         `json:"owner_email,omitempty"`
	ID          string         `json:"id,omitempty"`
}

// NamespaceConfig mirrors schema/tasks.zap NamespaceConfig. Retention is
// exposed as a Go Duration on the Go surface and marshalled to/from
// milliseconds on the wire (see retentionJSON below).
type NamespaceConfig struct {
	Retention time.Duration `json:"-"`
}

// retentionJSON captures the ms-granular wire shape without leaking
// temporal.io types.
type namespaceConfigWire struct {
	RetentionMs int64 `json:"retention_ms,omitempty"`
}

// MarshalJSON implements custom encoding for the wire shape.
func (n NamespaceConfig) MarshalJSON() ([]byte, error) {
	return jsonMarshal(namespaceConfigWire{RetentionMs: n.Retention.Milliseconds()})
}

// UnmarshalJSON implements custom decoding for the wire shape.
func (n *NamespaceConfig) UnmarshalJSON(b []byte) error {
	var w namespaceConfigWire
	if err := jsonUnmarshal(b, &w); err != nil {
		return err
	}
	n.Retention = time.Duration(w.RetentionMs) * time.Millisecond
	return nil
}

// NamespaceState mirrors the Int8 state enum.
type NamespaceState int8

const (
	NamespaceStateUnspecified NamespaceState = 0
	NamespaceStateRegistered  NamespaceState = 1
	NamespaceStateDeprecated  NamespaceState = 2
	NamespaceStateDeleted     NamespaceState = 3
)

// RegisterNamespaceRequest is the argument to RegisterNamespace. Mirrors
// schema/tasks.zap: server receives the full Namespace.
type RegisterNamespaceRequest struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	OwnerEmail  string        `json:"owner_email,omitempty"`
	Retention   time.Duration `json:"-"`
}

type registerNamespaceWire struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	OwnerEmail  string `json:"owner_email,omitempty"`
	RetentionMs int64  `json:"retention_ms,omitempty"`
}

type describeNamespaceRequest struct {
	Name string `json:"name"`
}

type describeNamespaceResponse struct {
	Namespace Namespace `json:"namespace"`
}

type listNamespacesRequest struct {
	PageSize      int32  `json:"page_size,omitempty"`
	NextPageToken []byte `json:"next_page_token,omitempty"`
}

// ListNamespacesResponse is returned by ListNamespaces.
type ListNamespacesResponse struct {
	Namespaces    []Namespace `json:"namespaces"`
	NextPageToken []byte      `json:"next_page_token,omitempty"`
}

// RegisterNamespace implements Client.
func (c *clientImpl) RegisterNamespace(ctx context.Context, req *RegisterNamespaceRequest) error {
	if req == nil || req.Name == "" {
		return errors.New("hanzo/tasks/client: namespace name is required")
	}
	wire := registerNamespaceWire{
		Name:        req.Name,
		Description: req.Description,
		OwnerEmail:  req.OwnerEmail,
		RetentionMs: req.Retention.Milliseconds(),
	}
	return c.roundTrip(ctx, opRegisterNamespace, wire, nil)
}

// DescribeNamespace implements Client.
func (c *clientImpl) DescribeNamespace(ctx context.Context, name string) (*Namespace, error) {
	if name == "" {
		return nil, errors.New("hanzo/tasks/client: namespace name is required")
	}
	var resp describeNamespaceResponse
	if err := c.roundTrip(ctx, opDescribeNamespace, describeNamespaceRequest{Name: name}, &resp); err != nil {
		return nil, err
	}
	out := resp.Namespace
	return &out, nil
}

// ListNamespaces implements Client.
func (c *clientImpl) ListNamespaces(ctx context.Context, pageSize int32, nextPageToken []byte) (*ListNamespacesResponse, error) {
	var resp ListNamespacesResponse
	req := listNamespacesRequest{PageSize: pageSize, NextPageToken: nextPageToken}
	if err := c.roundTrip(ctx, opListNamespaces, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
