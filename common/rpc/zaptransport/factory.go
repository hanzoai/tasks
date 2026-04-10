package zaptransport

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"

	"github.com/hanzoai/tasks/common"
	"github.com/hanzoai/tasks/common/config"
	"github.com/hanzoai/tasks/common/convert"
	"github.com/hanzoai/tasks/common/log"
	"github.com/hanzoai/tasks/common/log/tag"
	"github.com/hanzoai/tasks/common/membership"
	"github.com/hanzoai/tasks/common/metrics"
	"github.com/hanzoai/tasks/common/primitives"
	"github.com/hanzoai/tasks/common/rpc/encryption"
	"github.com/hanzoai/tasks/temporal/environment"
	"google.golang.org/grpc"
)

var _ common.RPCFactory = (*Factory)(nil)

// Factory is a ZAP-native RPCFactory. It creates ZAP connections instead of
// gRPC ones. The returned grpc.ClientConnInterface values are ZAP ClientConns.
type Factory struct {
	cfg         *config.Config
	serviceName primitives.ServiceName
	logger      log.Logger
	slogger     *slog.Logger
	metrics     metrics.Handler
	tlsFactory  encryption.TLSConfigProvider
	monitor     membership.Monitor

	frontendURL       string
	frontendHTTPURL   string
	frontendHTTPPort  int
	frontendTLSConfig *tls.Config

	listener    func() net.Listener
	connCache   sync.Map // addr -> *ClientConn
	nodeCounter uint64
	mu          sync.Mutex
}

// NewFactory creates a ZAP-backed RPCFactory.
func NewFactory(
	cfg *config.Config,
	sName primitives.ServiceName,
	logger log.Logger,
	metricsHandler metrics.Handler,
	tlsProvider encryption.TLSConfigProvider,
	frontendURL string,
	frontendHTTPURL string,
	frontendHTTPPort int,
	frontendTLSConfig *tls.Config,
	monitor membership.Monitor,
) *Factory {
	f := &Factory{
		cfg:               cfg,
		serviceName:       sName,
		logger:            logger,
		slogger:           slog.Default(),
		metrics:           metricsHandler,
		tlsFactory:        tlsProvider,
		monitor:           monitor,
		frontendURL:       frontendURL,
		frontendHTTPURL:   frontendHTTPURL,
		frontendHTTPPort:  frontendHTTPPort,
		frontendTLSConfig: frontendTLSConfig,
	}
	f.listener = sync.OnceValue(f.createListener)
	return f
}

func (f *Factory) GetFrontendGRPCServerOptions() ([]grpc.ServerOption, error) {
	return nil, nil
}

func (f *Factory) GetInternodeGRPCServerOptions() ([]grpc.ServerOption, error) {
	return nil, nil
}

func (f *Factory) GetGRPCListener() net.Listener {
	return f.listener()
}

func (f *Factory) createListener() net.Listener {
	rpcConfig := f.cfg.Services[string(f.serviceName)].RPC
	ip := getListenIP(&rpcConfig, f.logger)
	addr := net.JoinHostPort(ip.String(), convert.IntToString(rpcConfig.GRPCPort))

	lis, err := net.Listen("tcp", addr)
	if err != nil || lis == nil || lis.Addr() == nil {
		f.logger.Fatal("Failed to start ZAP listener", tag.Error(err), tag.Service(f.serviceName), tag.Address(addr))
	}
	f.logger.Info("Created ZAP listener", tag.Service(f.serviceName), tag.Address(addr))
	return lis
}

func (f *Factory) CreateRemoteFrontendGRPCConnection(rpcAddress string) grpc.ClientConnInterface {
	return f.getOrCreateConn(rpcAddress)
}

func (f *Factory) CreateLocalFrontendGRPCConnection() grpc.ClientConnInterface {
	return f.getOrCreateConn(f.frontendURL)
}

func (f *Factory) CreateHistoryGRPCConnection(rpcAddress string) grpc.ClientConnInterface {
	return f.getOrCreateConn(rpcAddress)
}

func (f *Factory) CreateMatchingGRPCConnection(rpcAddress string) grpc.ClientConnInterface {
	return f.getOrCreateConn(rpcAddress)
}

func (f *Factory) CreateLocalFrontendHTTPClient() (*common.FrontendHTTPClient, error) {
	scheme := "http"
	if f.frontendTLSConfig != nil {
		scheme = "https"
	}
	return &common.FrontendHTTPClient{
		Client:  http.Client{},
		Address: f.frontendHTTPURL,
		Scheme:  scheme,
	}, nil
}

func (f *Factory) getOrCreateConn(addr string) *ClientConn {
	if cached, ok := f.connCache.Load(addr); ok {
		return cached.(*ClientConn)
	}

	f.mu.Lock()
	f.nodeCounter++
	nodeID := fmt.Sprintf("tasks-client-%s-%d", f.serviceName, f.nodeCounter)
	f.mu.Unlock()

	cc := NewClientConn(addr, nodeID, f.slogger)
	actual, _ := f.connCache.LoadOrStore(addr, cc)
	return actual.(*ClientConn)
}

func getListenIP(cfg *config.RPC, logger log.Logger) net.IP {
	if cfg.BindOnLocalHost && len(cfg.BindOnIP) > 0 {
		logger.Fatal("ListenIP failed, bindOnLocalHost and bindOnIP are mutually exclusive")
		return nil
	}
	if cfg.BindOnLocalHost {
		return net.ParseIP(environment.GetLocalhostIP())
	}
	if len(cfg.BindOnIP) > 0 {
		ip := net.ParseIP(cfg.BindOnIP)
		if ip != nil {
			return ip
		}
		logger.Fatal("ListenIP failed, unable to parse bindOnIP value", tag.Address(cfg.BindOnIP))
		return nil
	}
	ip, err := config.ListenIP()
	if err != nil {
		logger.Fatal("ListenIP failed", tag.Error(err))
		return nil
	}
	return ip
}
