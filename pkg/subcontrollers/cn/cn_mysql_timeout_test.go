package cn

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/StarRocks/starrocks-kubernetes-operator/pkg/common/mysqlutil"
)

// An FE that accepts the connection and then says nothing must not hold the reconcile.
func TestQueryShowComputeNodesGivesUpOnSilentFE(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn) // kept open and never answered
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})

	previous := mysqlutil.RequestTimeout
	mysqlutil.RequestTimeout = 300 * time.Millisecond
	t.Cleanup(func() { mysqlutil.RequestTimeout = previous })

	_, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	// the executor joins service name and namespace with a dot: this spells 127.0.0.1
	executor := &SQLExecutor{FeServiceName: "127.0.0", FeServiceNamespace: "1", FeServicePort: port}

	start := time.Now()
	_, err = executor.QueryShowComputeNodes(context.Background(), nil)
	require.Error(t, err)
	require.Less(t, time.Since(start), 10*time.Second, "the query must give up after the request timeout")
}
