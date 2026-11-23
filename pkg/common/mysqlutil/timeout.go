package mysqlutil

import (
	"context"
	"time"
)

// RequestTimeout bounds one request to FE, from opening the connection to reading the last row. The operator
// only sends statements that FE answers at once. Without a bound, a connection to an FE that does not answer,
// for example while its Service still points at a pod that is gone, holds the reconcile of every cluster until
// the kernel gives up, which takes minutes.
var RequestTimeout = 15 * time.Second

// WithRequestTimeout returns a copy of ctx that expires after RequestTimeout.
func WithRequestTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, RequestTimeout)
}
