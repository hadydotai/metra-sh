package exchanges

import "time"

type deliverOutcome uint8

const (
	deliverOutcomeAllowed deliverOutcome = iota
	deliverOutcomeDelivered
	deliverOutcomeSkippedThrottle
	deliverOutcomeDroppedClosed
	deliverOutcomeDroppedBufferFull
)

// DeliveryMode controls how events are queued for a subscriber.
type DeliveryMode uint8

const (
	// DeliveryModeAll attempts to deliver every event. If the subscriber can't keep up,
	// the per-subscriber buffer may fill and events will be dropped.
	DeliveryModeAll DeliveryMode = iota
	// DeliveryModeLatestOnly keeps only the latest event for slow subscribers.
	// This bounds per-subscriber memory and avoids backlog growth under load.
	DeliveryModeLatestOnly
)

// SubscribeOptions control per-subscriber fanout behavior.
type SubscribeOptions struct {
	// Throttle drops events at the fanout layer if the subscriber is receiving events
	// faster than this interval. Zero means no throttle.
	Throttle time.Duration
	// Mode controls buffering semantics for slow subscribers.
	Mode DeliveryMode
}

func DefaultSubscribeOptions() SubscribeOptions {
	return SubscribeOptions{Throttle: 0, Mode: DeliveryModeAll}
}
