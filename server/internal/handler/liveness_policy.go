package handler

import "time"

// LivenessPolicy is the ONE place the liveness timings are written down.
//
// The rules live in the workspace protocol (skill `multica-protocol` §1.5 D) and
// describe behaviour in relative terms on purpose: agents must not depend on a
// minute count, and a table copied into a document drifts the moment a constant
// changes. Anything that needs the actual number reads these constants.
//
// Every threshold answers "how long is this state allowed to sit before the next
// actor is woken":
//
//	LivenessDeadEndIdle         a turn that left no successor (nothing in flight)
//	LivenessAddressedIdle       a turn that did name someone (that dispatch may
//	                            still be claimed)
//	LivenessReviewIdle          an `in_review` issue whose gate is missing or
//	                            superseded
//	LivenessWaitIdle            a sanctioned wait that named an agent, before the
//	                            ladder climbs
//	LivenessHumanRequestGrace   a sanctioned wait that asks a human, before the
//	                            owner is told (once, not repeated)
//	LivenessBatch               how many issues one tick may act on
type LivenessPolicy struct {
	DeadEndIdle       time.Duration
	AddressedIdle     time.Duration
	ReviewIdle        time.Duration
	WaitIdle          time.Duration
	HumanRequestGrace time.Duration
	Batch             int
}

// DefaultLivenessPolicy is the deployed policy.
func DefaultLivenessPolicy() LivenessPolicy {
	return LivenessPolicy{
		DeadEndIdle:       3 * time.Minute,
		AddressedIdle:     15 * time.Minute,
		ReviewIdle:        10 * time.Minute,
		WaitIdle:          20 * time.Minute,
		HumanRequestGrace: 10 * time.Minute,
		Batch:             5,
	}
}

// livenessPolicyValue is the policy the sweeps read. A package variable rather
// than a literal at each call site so a test can tighten a window without
// sleeping, and so there is exactly one numeric source to change.
var livenessPolicyValue = DefaultLivenessPolicy()

func (h *Handler) livenessPolicy() LivenessPolicy { return livenessPolicyValue }
