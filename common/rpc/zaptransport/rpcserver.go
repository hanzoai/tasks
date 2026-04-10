package zaptransport

import (
	"net"

	"google.golang.org/grpc"
)

// RPCServer is the interface that both *grpc.Server and *zaptransport.Server
// implement. It allows services to register handlers and serve connections
// regardless of the underlying transport.
type RPCServer interface {
	grpc.ServiceRegistrar
	Serve(lis net.Listener) error
	GracefulStop()
	Stop()
}

// Verify interface compliance at compile time.
var (
	_ RPCServer = (*grpc.Server)(nil)
	_ RPCServer = (*Server)(nil)
)
