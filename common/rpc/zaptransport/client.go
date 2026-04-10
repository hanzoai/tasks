package zaptransport

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/luxfi/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ClientConn implements grpc.ClientConnInterface over ZAP.
// It can be used anywhere a *grpc.ClientConn is expected for making
// unary RPC calls (which is all Temporal's internode traffic).
type ClientConn struct {
	node   *zap.Node
	peerID string
	addr   string
	logger *slog.Logger

	once sync.Once
	err  error
}

// Ensure ClientConn implements the interface.
var _ grpc.ClientConnInterface = (*ClientConn)(nil)

// NewClientConn creates a ZAP client connection to the given address.
// The peerID is the remote ZAP node ID (typically the host:port string).
func NewClientConn(addr string, nodeID string, logger *slog.Logger) *ClientConn {
	if logger == nil {
		logger = slog.Default()
	}
	return &ClientConn{
		addr:   addr,
		peerID: addr, // use addr as peer ID until handshake resolves it
		logger: logger,
		node: zap.NewNode(zap.NodeConfig{
			NodeID:      nodeID,
			ServiceType: "_tasks._tcp",
			Port:        0, // ephemeral port for client
			Logger:      logger,
			NoDiscovery: true,
		}),
	}
}

// connect lazily establishes the ZAP connection.
func (c *ClientConn) connect() error {
	c.once.Do(func() {
		if err := c.node.Start(); err != nil {
			c.err = fmt.Errorf("zap client start: %w", err)
			return
		}
		if err := c.node.ConnectDirect(c.addr); err != nil {
			c.err = fmt.Errorf("zap connect to %s: %w", c.addr, err)
			return
		}
		// After ConnectDirect, the actual peer ID is resolved via handshake.
		// Update peerID from the connected peers list.
		peers := c.node.Peers()
		if len(peers) > 0 {
			c.peerID = peers[0]
		}
	})
	return c.err
}

// Invoke implements grpc.ClientConnInterface for unary RPCs.
func (c *ClientConn) Invoke(ctx context.Context, method string, args any, reply any, opts ...grpc.CallOption) error {
	if err := c.connect(); err != nil {
		return status.Errorf(codes.Unavailable, "zap connect: %v", err)
	}

	// Marshal request.
	reqMsg, ok := args.(proto.Message)
	if !ok {
		return status.Errorf(codes.Internal, "args is not proto.Message: %T", args)
	}
	reqBytes, err := proto.Marshal(reqMsg)
	if err != nil {
		return status.Errorf(codes.Internal, "marshal request: %v", err)
	}

	// Extract outgoing metadata.
	var md metadata.MD
	if outMD, ok := metadata.FromOutgoingContext(ctx); ok {
		md = outMD
	}

	// Build ZAP request message.
	zapReq := buildUnaryRequest(method, reqBytes, md)

	// Call and wait for response.
	zapResp, err := c.node.Call(ctx, c.peerID, zapReq)
	if err != nil {
		return status.Errorf(codes.Unavailable, "zap call: %v", err)
	}

	// Parse response.
	root := zapResp.Root()
	msgType := zapResp.Flags() >> 8

	if msgType == OpcodeError {
		code := codes.Code(root.Uint32(0))
		msg := root.Text(4)
		return status.Error(code, msg)
	}

	// Unary response: field 0 = response proto bytes, field 8 = status code.
	respBytes := root.Bytes(0)
	statusCode := codes.Code(root.Uint32(8))
	if statusCode != codes.OK {
		return status.Error(statusCode, "server error")
	}

	// Unmarshal into reply.
	replyMsg, ok := reply.(proto.Message)
	if !ok {
		return status.Errorf(codes.Internal, "reply is not proto.Message: %T", reply)
	}
	if err := proto.Unmarshal(respBytes, replyMsg); err != nil {
		return status.Errorf(codes.Internal, "unmarshal response: %v", err)
	}

	return nil
}

// NewStream implements grpc.ClientConnInterface for streaming RPCs.
// Temporal's internode communication is exclusively unary, so streaming
// is not needed. This returns Unimplemented.
func (c *ClientConn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, status.Error(codes.Unimplemented, "zap transport does not support streaming")
}

// Close shuts down the underlying ZAP node.
func (c *ClientConn) Close() {
	c.node.Stop()
}

// ResetConnectBackoff is a no-op for ZAP (no backoff state to reset).
func (c *ClientConn) ResetConnectBackoff() {}

// Target returns the address this client is connected to.
func (c *ClientConn) Target() string {
	return c.addr
}
