// Package rpcstat counts RPC requests per caller: a caller puts a counter in the context and
// the RPC clients add one per request they send, so the scanner can log its calls per chain
// and mode without counting what other loops on the same providers do.
package rpcstat

import (
	"context"
	"sync/atomic"
)

type key struct{}

// With returns a context whose RPC requests are counted in c.
func With(ctx context.Context, c *atomic.Int64) context.Context {
	return context.WithValue(ctx, key{}, c)
}

// Count adds one request to the context's counter, if any.
func Count(ctx context.Context) {
	if c, ok := ctx.Value(key{}).(*atomic.Int64); ok {
		c.Add(1)
	}
}
