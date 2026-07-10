package services

import (
	"context"
	"sync/atomic"
)

// runtime_context.go mirrors the site's services.SetRootContext /
// services.RootContext pair so the agent's long-lived goroutines can
// observe the SIGTERM-cancelled root context the same way without
// every Start* caller having to thread the context through by hand.
//
// main() calls SetRootContext exactly once near the top of startup
// (after signal.NotifyContext); package-level services that don't
// receive a ctx in their signature (or that need it from a deeper
// callsite) read it via RootContext.
//
// Stored as atomic.Pointer[context.Context] so the getter is
// lock-free on the hot path and the setter is safe across the boot
// goroutines that race to publish it. RootContext falls back to
// context.Background() before SetRootContext lands so callers
// during early-boot still see a usable (non-cancellable) context
// rather than nil — boot-order bugs were the reason this needs a
// fallback and not a nil check.

var rootCtx atomic.Pointer[context.Context]

// SetRootContext publishes the agent's shutdown-aware root context so
// services without a ctx parameter can observe SIGTERM. Idempotent;
// late callers overwrite earlier ones. Should be invoked exactly
// once from main() before any Start* call.
func SetRootContext(ctx context.Context) {
	if ctx == nil {
		return
	}
	rootCtx.Store(&ctx)
}

// RootContext returns the published shutdown-aware context, falling
// back to context.Background() if SetRootContext hasn't been called
// yet. Callers should treat the returned context as read-only —
// they MAY derive children via context.WithCancel / WithTimeout
// but MUST NOT mutate the root itself.
func RootContext() context.Context {
	if p := rootCtx.Load(); p != nil && *p != nil {
		return *p
	}
	return context.Background()
}
