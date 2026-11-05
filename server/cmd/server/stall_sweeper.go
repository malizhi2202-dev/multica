package main

import (
	"context"
	"time"

	"github.com/multica-ai/multica/server/internal/handler"
)

// stallSweepInterval is how often stalled issues are re-armed. The sweep is a
// couple of indexed queries on the common path (nothing to do), so a short
// interval is cheaper than the tokens a frozen issue burns by sitting idle.
const stallSweepInterval = 2 * time.Minute

// runStallSweeper keeps active issues moving without a human wake-up: it
// corrects `blocked` rows that have a run in flight, re-arms leaf issues that
// stopped with no run, and reminds assignees of stage children that never
// started. It runs once at startup so a restart immediately recovers whatever
// went quiet while the server was down.
func runStallSweeper(ctx context.Context, h *handler.Handler) {
	if h == nil {
		return
	}
	h.SweepStalledIssues(ctx)
	runPeriodicSweep(ctx, stallSweepInterval, func() {
		h.SweepStalledIssues(ctx)
	})
}
