// Package zaptransport implements a gRPC-compatible server and client backed by
// luxfi/zap zero-copy binary RPC. Protobuf serialization is preserved -- only
// the transport layer changes from HTTP/2 (gRPC) to raw TCP (ZAP).
//
// Generated _grpc.pb.go code continues to work unmodified because ZAP server
// implements grpc.ServiceRegistrar and ZAP client implements
// grpc.ClientConnInterface.
package zaptransport

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/luxfi/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Opcodes used over ZAP wire.
const (
	// OpcodeUnary is a unary RPC call. Payload layout:
	//   root object field 0: method name (text)
	//   root object field 8: request protobuf bytes
	//   root object field 16: metadata key-value pairs (text, alternating k/v)
	OpcodeUnary uint16 = 1

	// OpcodeError is an error response.
	//   root object field 0: gRPC status code (uint32)
	//   root object field 4: error message (text)
	OpcodeError uint16 = 2
)

// methodHandler mirrors grpc._MethodDesc.Handler -- the function signature
// that protobuf-generated code registers for each unary method.
type methodHandler = func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error)

type serviceEntry struct {
	impl    any
	methods map[string]methodHandler // "/pkg.Service/Method" -> handler
}

// Server is a ZAP-backed server that implements grpc.ServiceRegistrar.
// It serves protobuf-over-ZAP for internode communication.
type Server struct {
	node     *zap.Node
	listener net.Listener

	mu       sync.RWMutex
	services map[string]*serviceEntry // service name -> entry

	interceptor grpc.UnaryServerInterceptor // optional chain
	logger      *slog.Logger
	started     bool

	// health tracking
	healthMu     sync.RWMutex
	healthStatus map[string]bool // service -> serving
}

// ServerOption configures the ZAP server.
type ServerOption func(*Server)

// WithInterceptor sets a unary server interceptor chain.
func WithInterceptor(i grpc.UnaryServerInterceptor) ServerOption {
	return func(s *Server) { s.interceptor = i }
}

// WithLogger sets the server logger.
func WithLogger(l *slog.Logger) ServerOption {
	return func(s *Server) { s.logger = l }
}

// NewServer creates a ZAP server that listens on the given listener.
func NewServer(lis net.Listener, nodeID string, opts ...ServerOption) *Server {
	s := &Server{
		listener:     lis,
		services:     make(map[string]*serviceEntry),
		healthStatus: make(map[string]bool),
		logger:       slog.Default(),
	}
	for _, o := range opts {
		o(s)
	}

	port := 0
	if addr, ok := lis.Addr().(*net.TCPAddr); ok {
		port = addr.Port
	}

	s.node = zap.NewNode(zap.NodeConfig{
		NodeID:      nodeID,
		ServiceType: "_tasks._tcp",
		Port:        port,
		Logger:      s.logger,
		NoDiscovery: true, // tasks uses membership ring, not mDNS
	})
	return s
}

// RegisterService implements grpc.ServiceRegistrar.
func (s *Server) RegisterService(desc *grpc.ServiceDesc, impl any) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry := &serviceEntry{
		impl:    impl,
		methods: make(map[string]methodHandler),
	}
	for i := range desc.Methods {
		m := &desc.Methods[i]
		fullMethod := fmt.Sprintf("/%s/%s", desc.ServiceName, m.MethodName)
		entry.methods[fullMethod] = m.Handler
	}
	s.services[desc.ServiceName] = entry
}

// Serve starts accepting connections. Blocks until Stop is called.
func (s *Server) Serve(lis net.Listener) error {
	if lis != nil {
		s.listener = lis
	}
	s.node.Handle(OpcodeUnary, s.handleUnary)
	if err := s.node.Start(); err != nil {
		return fmt.Errorf("zap server start: %w", err)
	}
	s.started = true

	// Accept loop -- ZAP node handles connections internally,
	// but we also accept on our listener and hand off to ConnectDirect.
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.logger.Error("accept error", "error", err)
			continue
		}
		// Hand off the raw conn to the ZAP node's connection handling.
		// ZAP expects to dial peers, but for server-side accepts we
		// close the accepted conn and let ZAP's internal listener handle it.
		// Since we configured the ZAP node on the same port, the ZAP node's
		// own listener is actually handling accepts. We just need to not
		// double-bind. Close this conn -- ZAP already accepted it.
		conn.Close()
	}
}

// handleUnary dispatches a unary RPC call received over ZAP.
func (s *Server) handleUnary(ctx context.Context, from string, msg *zap.Message) (*zap.Message, error) {
	root := msg.Root()

	method := root.Text(0)
	reqBytes := root.Bytes(8)
	mdText := root.Text(16)

	// Parse metadata from alternating k/v pairs.
	if mdText != "" {
		md := parseMetadata(mdText)
		ctx = metadata.NewIncomingContext(ctx, md)
	}

	// Look up handler.
	s.mu.RLock()
	var handler methodHandler
	var impl any
	for _, svc := range s.services {
		if h, ok := svc.methods[method]; ok {
			handler = h
			impl = svc.impl
			break
		}
	}
	s.mu.RUnlock()

	if handler == nil {
		return buildErrorResponse(codes.Unimplemented, fmt.Sprintf("method %s not found", method)), nil
	}

	// Decoder for the protobuf request.
	dec := func(out any) error {
		msg, ok := out.(proto.Message)
		if !ok {
			return fmt.Errorf("expected proto.Message, got %T", out)
		}
		return proto.Unmarshal(reqBytes, msg)
	}

	resp, err := handler(impl, ctx, dec, s.interceptor)
	if err != nil {
		st, _ := status.FromError(err)
		return buildErrorResponse(st.Code(), st.Message()), nil
	}

	// Serialize response.
	respMsg, ok := resp.(proto.Message)
	if !ok {
		return buildErrorResponse(codes.Internal, "handler returned non-proto response"), nil
	}
	respBytes, err := proto.Marshal(respMsg)
	if err != nil {
		return buildErrorResponse(codes.Internal, fmt.Sprintf("marshal response: %v", err)), nil
	}

	return buildUnaryResponse(respBytes), nil
}

// GracefulStop drains and stops the server.
func (s *Server) GracefulStop() {
	// Give in-flight requests a moment, then stop.
	time.Sleep(100 * time.Millisecond)
	s.node.Stop()
}

// Stop immediately stops the server.
func (s *Server) Stop() {
	s.node.Stop()
}

// --- wire helpers ---

// buildUnaryResponse builds a ZAP message containing a protobuf response.
func buildUnaryResponse(respBytes []byte) *zap.Message {
	b := zap.NewBuilder(len(respBytes) + 64)
	obj := b.StartObject(24)
	obj.SetBytes(0, respBytes) // response proto bytes at field 0
	obj.SetUint32(8, 0)       // status OK
	obj.FinishAsRoot()
	flags := uint16(OpcodeUnary) << 8
	data := b.FinishWithFlags(flags)
	msg, _ := zap.Parse(data)
	return msg
}

// buildErrorResponse builds a ZAP error message.
func buildErrorResponse(code codes.Code, message string) *zap.Message {
	b := zap.NewBuilder(len(message) + 64)
	obj := b.StartObject(24)
	obj.SetUint32(0, uint32(code))
	obj.SetText(4, message)
	obj.FinishAsRoot()
	flags := uint16(OpcodeError) << 8
	data := b.FinishWithFlags(flags)
	msg, _ := zap.Parse(data)
	return msg
}

// buildUnaryRequest builds a ZAP message for a unary RPC request.
func buildUnaryRequest(method string, reqBytes []byte, md metadata.MD) *zap.Message {
	mdStr := encodeMetadata(md)
	b := zap.NewBuilder(len(method) + len(reqBytes) + len(mdStr) + 128)
	obj := b.StartObject(32)
	obj.SetText(0, method)
	obj.SetBytes(8, reqBytes)
	obj.SetText(16, mdStr)
	obj.FinishAsRoot()
	flags := uint16(OpcodeUnary) << 8
	data := b.FinishWithFlags(flags)
	msg, _ := zap.Parse(data)
	return msg
}

// parseMetadata parses "k1\x00v1\x00k2\x00v2" into metadata.MD.
func parseMetadata(s string) metadata.MD {
	md := metadata.MD{}
	i := 0
	for i < len(s) {
		// find key end
		kEnd := i
		for kEnd < len(s) && s[kEnd] != 0 {
			kEnd++
		}
		if kEnd >= len(s) {
			break
		}
		key := s[i:kEnd]
		// find value end
		vStart := kEnd + 1
		vEnd := vStart
		for vEnd < len(s) && s[vEnd] != 0 {
			vEnd++
		}
		val := s[vStart:vEnd]
		md.Append(key, val)
		i = vEnd + 1
	}
	return md
}

// encodeMetadata encodes metadata.MD as "k1\x00v1\x00k2\x00v2\x00".
func encodeMetadata(md metadata.MD) string {
	if md == nil || len(md) == 0 {
		return ""
	}
	var buf []byte
	for k, vals := range md {
		for _, v := range vals {
			buf = append(buf, k...)
			buf = append(buf, 0)
			buf = append(buf, v...)
			buf = append(buf, 0)
		}
	}
	return string(buf)
}

// silence the import
var _ = binary.LittleEndian
