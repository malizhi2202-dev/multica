package main

import (
	"context"
	"time"

	"github.com/multica-ai/multica/server/internal/handler"
)

// livenessSweepInterval is how often stalled issues are repaired. The sweep is
// one indexed query on the common path (nothing to act on), so a short interval
// is cheaper than the tokens a frozen issue burns by sitting idle.
const livenessSweepInterval = time.Minute

// runLivenessSweeper keeps delivery moving without a human wake-up: every
// non-terminal issue must either have a run, wait on a sanctioned state, or be
// climbed up the ladder. It runs once at startup so a restart immediately
// recovers whatever went quiet while the server was down.
func runLivenessSweeper(ctx context.Context, h *handler.Handler) {
	if h == nil {
		return
	}
	h.SweepLiveness(ctx)
	runPeriodicSweep(ctx, livenessSweepInterval, func() {
		h.SweepLiveness(ctx)
	})
}
