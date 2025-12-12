package exchanges

import (
	"context"
	"time"
)

// unexported, and sealed
type qualificationStage interface{ bsmcs() }

// NOTE(@hadydotai): Keen eyes will notice the Qualified/Unqualified phantom tags, a page from my OCaml days.
// Go doesn’t have opaque/phantom types, but we can simulate a “state machine” using zero-size marker structs
// and a generic type parameter: BitstampMarketChannel[T]. The actual value is still just a string.
//
// The goal is to make illegal states harder to represent. A BitstampMarketChannel starts Unqualified because
// Bitstamp won’t accept the base channel name by itself, it must be suffixed with `_[market]`. So we gate the
// transition via `WithMarket`, which only exists on Unqualified channels and returns a Qualified one after
// validating the market against the supported set.
//
// Important caveat: this is guidance + compile-time nudging. Anyone can still
// explicitly convert a string into a Qualified channel and bypass validation. The payoff is that the “right”
// path is obvious and type-checked, and the wrong path is noisy and intentional.
//
// The way out of this is 1) sealing the states by having them implement a non-exported interface that's only
// accessible from within the package, 2) turning BitstampMarketChannel to a struct instead of a string
// to completely cut off the type casting path on a compiler level.
//
// (1) we already do with the stage interface, (2) is unnecessary here, might be useful for a public facing library
// but this is internal code.
//
// Also: generics overhead here is effectively zero cost at runtime as well, Go generics are not fully monomorphized,
// but We’re not feeling the cost here because we're not doing dynamic dispatching on T, and we’re
// not computing with T. It’s only used to create distinct types and gate methods. The trade-offs are mostly
// API surface, and potential code size/compile-time effects, not per-message runtime cost.

// Qualified tags any data that has crossed a validation boundary
type Qualified struct{}

func (Qualified) bsmcs() {}

// Unqualified tags any data that hasn't yet crossed a validation boundary and should be considered untrusted
type Unqualified struct{}

func (Unqualified) bsmcs() {}

type ConfigFunc[T any] func(*T) *T

// Trade represents a unified trade event across all exchanges.
type Trade struct {
	ID        string    `json:"id"`
	Exchange  string    `json:"exchange"`
	Market    string    `json:"market"`
	Price     string    `json:"price"`
	Amount    string    `json:"amount"`
	Side      string    `json:"side"` // "buy" or "sell"
	Timestamp time.Time `json:"timestamp"`
}

// Event is a normalized payload emitted by an exchange integration.
type Event struct {
	Exchange string
	Stream   string
	Payload  any
	Time     time.Time
}

// Exchange represents a streaming integration that emits Events via Start.
type Exchange interface {
	Name() string
	Start(ctx context.Context, out chan<- *Event) error
}
