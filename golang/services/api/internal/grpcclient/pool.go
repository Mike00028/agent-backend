package grpcclient

import (
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// Pool holds a round-robin set of gRPC client connections to the Python
// LangGraph service. Use New to create and Close to release connections.
type Pool struct {
	conns []*grpc.ClientConn
	idx   atomic.Uint64
}

// New creates a Pool of `size` connections to PYTHON_GRPC_ADDR (default :50051).
func New(size int) (*Pool, error) {
	addr := os.Getenv("PYTHON_GRPC_ADDR")
	if addr == "" {
		addr = "localhost:50051"
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                120 * time.Second, // Ping every 2 min for long-running streams
			Timeout:             10 * time.Second,
			PermitWithoutStream: false, // Don't ping when no active RPC
		}),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(10 * 1024 * 1024),
		),
	}

	p := &Pool{conns: make([]*grpc.ClientConn, size)}
	for i := range size {
		conn, err := grpc.NewClient(addr, opts...)
		if err != nil {
			return nil, fmt.Errorf("grpcclient: dial[%d]: %w", i, err)
		}
		p.conns[i] = conn
	}
	return p, nil
}

// Next returns the next connection in round-robin order.
func (p *Pool) Next() *grpc.ClientConn {
	i := p.idx.Add(1) % uint64(len(p.conns))
	return p.conns[i]
}

// Close releases all connections in the pool.
func (p *Pool) Close() {
	for _, c := range p.conns {
		_ = c.Close()
	}
}
