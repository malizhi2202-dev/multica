package main

import (
	"context"
	"time"

	"github.com/multica-ai/multica/server/internal/handler"
)

// barrierSweepInterval bounds how long a gated, idle issue can sit before it is
// advanced to `done`. The wait is cheap (one indexed query per tick) and the
// effect is what lets a parent's stage barrier close without a human.
const barrierSweepInterval = 2 * time.Minute

// runBarrierSweeper advances issues whose acceptance gate was posted but whose
// status never reached done, so a finished child cannot hold its stage open
// forever. It runs once at startup (restart recovery) and then on a ticker.
func runBarrierSweeper(ctx context.Context, h *handler.Handler) {
	if h == nil {
		return
	}
	h.SweepBarrierLiveness(ctx)
	runPeriodicSweep(ctx, barrierSweepInterval, func() {
		h.SweepBarrierLiveness(ctx)
	})
}
