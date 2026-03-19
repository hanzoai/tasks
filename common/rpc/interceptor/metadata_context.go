package interceptor

import (
	"context"

	"github.com/hanzoai/tasks/common/contextutil"
	"google.golang.org/grpc"
)

type MetadataContextInterceptor struct{}

// NewMetadataContextInterceptor creates a new MetadataContextInterceptor
func NewMetadataContextInterceptor() *MetadataContextInterceptor {
	return &MetadataContextInterceptor{}
}

// Intercept adds metadata context to all incoming gRPC requests
func (m *MetadataContextInterceptor) Intercept(
	ctx context.Context,
	req any,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (any, error) {
	ctx = contextutil.WithMetadataContext(ctx)
	return handler(ctx, req)
}
