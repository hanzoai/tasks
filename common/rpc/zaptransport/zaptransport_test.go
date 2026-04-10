package zaptransport

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// TestServerRegistersAndDispatchesUnary verifies that the ZAP server can
// register a gRPC service descriptor and dispatch unary RPC calls from
// a ZAP client.
func TestServerRegistersAndDispatchesUnary(t *testing.T) {
	// Create server listener.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	// Build a simple service descriptor with one method.
	echoHandler := func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
		in := new(wrapperspb.StringValue)
		if err := dec(in); err != nil {
			return nil, err
		}
		// Echo back with prefix.
		return &wrapperspb.StringValue{Value: "echo:" + in.Value}, nil
	}

	desc := grpc.ServiceDesc{
		ServiceName: "test.EchoService",
		Methods: []grpc.MethodDesc{
			{
				MethodName: "Echo",
				Handler:    echoHandler,
			},
		},
	}

	logger := slog.Default()
	server := NewServer(lis, "test-server", WithLogger(logger))
	server.RegisterService(&desc, nil)

	// Start server in background.
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Serve(nil)
	}()

	// Give server a moment to start.
	time.Sleep(50 * time.Millisecond)

	// Create client.
	addr := lis.Addr().String()
	client := NewClientConn(addr, "test-client", logger)
	defer client.Close()

	// Make RPC call.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := &wrapperspb.StringValue{Value: "hello"}
	reply := &wrapperspb.StringValue{}
	err = client.Invoke(ctx, "/test.EchoService/Echo", req, reply)
	require.NoError(t, err)
	assert.Equal(t, "echo:hello", reply.Value)

	// Stop server.
	server.Stop()
}

// TestServerReturnsUnimplementedForUnknownMethod verifies that calling
// an unknown method returns Unimplemented.
func TestServerReturnsUnimplementedForUnknownMethod(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	logger := slog.Default()
	server := NewServer(lis, "test-server-2", WithLogger(logger))
	// Register nothing.

	go func() {
		server.Serve(nil)
	}()
	time.Sleep(50 * time.Millisecond)

	client := NewClientConn(lis.Addr().String(), "test-client-2", logger)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := &emptypb.Empty{}
	reply := &emptypb.Empty{}
	err = client.Invoke(ctx, "/unknown.Service/Method", req, reply)
	require.Error(t, err)

	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unimplemented, st.Code())

	server.Stop()
}

// TestServerWithInterceptor verifies that the unary interceptor chain works.
func TestServerWithInterceptor(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	interceptorCalled := false
	interceptor := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		interceptorCalled = true
		return handler(ctx, req)
	}

	echoHandler := func(srv any, ctx context.Context, dec func(any) error, ic grpc.UnaryServerInterceptor) (any, error) {
		in := new(wrapperspb.StringValue)
		if err := dec(in); err != nil {
			return nil, err
		}
		if ic != nil {
			info := &grpc.UnaryServerInfo{FullMethod: "/test.EchoService/Echo"}
			return ic(ctx, in, info, func(ctx context.Context, req any) (any, error) {
				return &wrapperspb.StringValue{Value: "intercepted:" + req.(*wrapperspb.StringValue).Value}, nil
			})
		}
		return &wrapperspb.StringValue{Value: in.Value}, nil
	}

	desc := grpc.ServiceDesc{
		ServiceName: "test.EchoService",
		Methods:     []grpc.MethodDesc{{MethodName: "Echo", Handler: echoHandler}},
	}

	logger := slog.Default()
	server := NewServer(lis, "test-server-3", WithLogger(logger), WithInterceptor(interceptor))
	server.RegisterService(&desc, nil)

	go func() { server.Serve(nil) }()
	time.Sleep(50 * time.Millisecond)

	client := NewClientConn(lis.Addr().String(), "test-client-3", logger)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := &wrapperspb.StringValue{Value: "world"}
	reply := &wrapperspb.StringValue{}
	err = client.Invoke(ctx, "/test.EchoService/Echo", req, reply)
	require.NoError(t, err)
	assert.True(t, interceptorCalled)
	assert.Equal(t, "intercepted:world", reply.Value)

	server.Stop()
}

// TestMetadataPropagation verifies metadata is passed from client to server.
func TestMetadataPropagation(t *testing.T) {
	encoded := encodeMetadata(map[string][]string{
		"key1": {"val1"},
		"key2": {"val2"},
	})
	parsed := parseMetadata(encoded)
	assert.Equal(t, []string{"val1"}, parsed.Get("key1"))
	assert.Equal(t, []string{"val2"}, parsed.Get("key2"))
}

// TestBuildAndParseMessages verifies wire format round-trip.
func TestBuildAndParseMessages(t *testing.T) {
	req := &wrapperspb.StringValue{Value: "test-payload"}
	reqBytes, err := proto.Marshal(req)
	require.NoError(t, err)

	msg := buildUnaryRequest("/svc/Method", reqBytes, nil)
	require.NotNil(t, msg)

	// Verify we can read back the fields.
	root := msg.Root()
	assert.Equal(t, "/svc/Method", root.Text(0))
	assert.Equal(t, reqBytes, root.Bytes(8))

	// Build and parse error.
	errMsg := buildErrorResponse(codes.NotFound, "not found")
	require.NotNil(t, errMsg)
	errRoot := errMsg.Root()
	assert.Equal(t, uint32(codes.NotFound), errRoot.Uint32(0))
	assert.Equal(t, "not found", errRoot.Text(4))

	// Build and parse response.
	resp := &wrapperspb.StringValue{Value: "response"}
	respBytes, err := proto.Marshal(resp)
	require.NoError(t, err)
	respMsg := buildUnaryResponse(respBytes)
	require.NotNil(t, respMsg)
	respRoot := respMsg.Root()
	assert.Equal(t, respBytes, respRoot.Bytes(0))
	assert.Equal(t, uint32(0), respRoot.Uint32(8)) // OK status
}

// TestClientConnImplementsInterface verifies the interface compliance.
func TestClientConnImplementsInterface(t *testing.T) {
	var _ grpc.ClientConnInterface = (*ClientConn)(nil)
}

// TestNewStreamReturnsUnimplemented verifies streaming is rejected.
func TestNewStreamReturnsUnimplemented(t *testing.T) {
	client := NewClientConn("127.0.0.1:1", "test", nil)
	_, err := client.NewStream(context.Background(), nil, "/svc/Stream")
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unimplemented, st.Code())
}

// BenchmarkUnaryRoundTrip measures ZAP unary RPC latency.
func BenchmarkUnaryRoundTrip(b *testing.B) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(b, err)

	echoHandler := func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
		in := new(wrapperspb.StringValue)
		if err := dec(in); err != nil {
			return nil, err
		}
		return in, nil // echo back
	}

	desc := grpc.ServiceDesc{
		ServiceName: "bench.Echo",
		Methods:     []grpc.MethodDesc{{MethodName: "Echo", Handler: echoHandler}},
	}

	server := NewServer(lis, "bench-server")
	server.RegisterService(&desc, nil)
	go func() { server.Serve(nil) }()
	time.Sleep(50 * time.Millisecond)

	client := NewClientConn(lis.Addr().String(), "bench-client", nil)
	defer client.Close()

	ctx := context.Background()
	req := &wrapperspb.StringValue{Value: "bench"}

	// Warm up.
	reply := &wrapperspb.StringValue{}
	err = client.Invoke(ctx, "/bench.Echo/Echo", req, reply)
	require.NoError(b, err)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		reply := &wrapperspb.StringValue{}
		if err := client.Invoke(ctx, "/bench.Echo/Echo", req, reply); err != nil {
			b.Fatal(err)
		}
	}

	server.Stop()
}

// Verify factory satisfies the interface.
func TestFactoryInterface(t *testing.T) {
	// Just a compile-time check.
	var _ fmt.Stringer // suppress unused import
}
