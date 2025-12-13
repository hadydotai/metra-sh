package exchanges

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gonanoid "github.com/matoous/go-nanoid/v2"
)

const (
	bitstampExchangeName     = "bitstamp"
	bitstampDefaultWSURL     = "wss://ws.bitstamp.net"
	bitstampSubscribeEvent   = "bts:subscribe"
	bitstampUnsubscribeEvent = "bts:unsubscribe"
)

var (
	ErrBitstampUnsupportedMarket = errors.New("bitstamp: subscription not found")
	ErrBitstampRouteKeyMissing   = errors.New("bitstamp: route key missing")
)

var (
	bitstampHeartbeatPayload = []byte(`{"event":"bts:heartbeat"}`)
	// bitstampRequestReconnectPayload = []byte(`{"event":"bts:request_reconnect"}`)
)

type BitstampMarketChannel[T qualificationStage] string

const (
	BitstampLiveTickerChannel BitstampMarketChannel[Unqualified] = "live_trades"
	// BitstampLiveOrdersChannel          BitstampMarketChannel[Unqualified] = "live_orders"
	// BitstampLiveOrderBookChannel       BitstampMarketChannel[Unqualified] = "order_book"
	// BitstampLiveDetailOrderBookChannel BitstampMarketChannel[Unqualified] = "detail_order_book"
	// BitstampLiveFullOrderBookChannel   BitstampMarketChannel[Unqualified] = "diff_order_book"
	// BitstampLiveFundingRateChannel     BitstampMarketChannel[Unqualified] = "funding_rate"
)

func (channel BitstampMarketChannel[UnQualified]) WithMarket(market string) (BitstampMarketChannel[Qualified], error) {
	if _, ok := bitstampMarkets[market]; !ok {
		return "", fmt.Errorf("unknown bitstamp market %q: %w", market, ErrBitstampUnsupportedMarket)
	}
	return BitstampMarketChannel[Qualified](fmt.Sprintf("%s_%s", string(channel), market)), nil
}

func (channel BitstampMarketChannel[Qualified]) String() string {
	return string(channel)
}

// BitstampConfig wires Bitstamp public subscriptions into the manager.
type BitstampConfig struct {
	WebsocketURL       string
	Streams            []BitstampMarketChannel[Qualified]
	HeartbeatInterval  time.Duration
	SubscriptionBuffer int
	Logger             *slog.Logger
}

func bitstampDefaultConfig() BitstampConfig {
	return BitstampConfig{
		WebsocketURL:       bitstampDefaultWSURL,
		HeartbeatInterval:  30 * time.Second,
		SubscriptionBuffer: 256,
		Logger:             slog.Default(),
	}
}

func BitstampWebsocketURL(wssURL string) ConfigFunc[BitstampConfig] {
	return func(bc *BitstampConfig) *BitstampConfig {
		bc.WebsocketURL = wssURL
		return bc
	}
}

func BitstampSubscribe(stream BitstampMarketChannel[Qualified]) ConfigFunc[BitstampConfig] {
	return func(bc *BitstampConfig) *BitstampConfig {
		bc.Streams = append(bc.Streams, stream)
		return bc
	}
}

func BitstampSubscriptionBuffer(size int) ConfigFunc[BitstampConfig] {
	return func(bc *BitstampConfig) *BitstampConfig {
		bc.SubscriptionBuffer = size
		return bc
	}
}

func BitstampLogger(logger *slog.Logger) ConfigFunc[BitstampConfig] {
	return func(bc *BitstampConfig) *BitstampConfig {
		bc.Logger = logger
		return bc
	}
}

// BitstampSubscription multiplexes a single upstream Bitstamp channel to multiple downstream consumers.
type BitstampSubscription struct {
	id     string
	stream BitstampMarketChannel[Qualified]
	ch     chan *BitstampEvent

	closeOnce sync.Once
	closeFn   func()

	mu     sync.RWMutex
	closed bool
}

func (s *BitstampSubscription) ID() string { return s.id }

func (s *BitstampSubscription) Stream() BitstampMarketChannel[Qualified] { return s.stream }

func (s *BitstampSubscription) C() <-chan *BitstampEvent { return s.ch }

func (s *BitstampSubscription) Close() error {
	s.closeOnce.Do(func() {
		if s.closeFn != nil {
			s.closeFn()
		}
	})
	return nil
}

func (s *BitstampSubscription) deliver(evt *BitstampEvent) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return false
	}
	select {
	case s.ch <- evt:
		return true
	default:
		return false
	}
}

type bitstampChannelRoute struct {
	stream      BitstampMarketChannel[Qualified]
	subscribers map[string]*BitstampSubscription
}

// Bitstamp implements the Exchange interface.
type Bitstamp struct {
	cfg    BitstampConfig
	logger *slog.Logger // NOTE(@hadydotai): Technically don't need this here but for convinence

	manager *connectionManager
	readCh  chan socketMessage

	routesMu sync.RWMutex
	routes   map[string]*bitstampChannelRoute

	connected atomic.Bool
}

// NewBitstamp wires a Bitstamp exchange based on the provided config.
func NewBitstamp(cfgFns ...ConfigFunc[BitstampConfig]) (*Bitstamp, error) {
	cfg := bitstampDefaultConfig()
	for _, fn := range cfgFns {
		fn(&cfg)
	}

	maxConnectionAge := 89 * 24 * time.Hour
	readBuffer := max(cfg.SubscriptionBuffer*4, 1024)
	readCh := make(chan socketMessage, readBuffer)

	opts := connectionManagerConfig{
		heartbeat: &socketHeartBeat{
			interval: cfg.HeartbeatInterval,
			payload:  bitstampHeartbeatPayload,
		},
		readCh:           readCh,
		maxConnectionAge: &maxConnectionAge,
		readTimeout:      60 * time.Second,
		WriteTimeout:     10 * time.Second,
		backoff: backoffConfig{
			initial:    750 * time.Millisecond,
			maxTries:   2 * time.Minute,
			multiplier: 1.7,
			jitter:     0.4,
		},
		logger: cfg.Logger.With("exchange", "bitstamp"),
	}

	manager, err := newConnectionManager(cfg.WebsocketURL, opts)
	if err != nil {
		return nil, err
	}

	return &Bitstamp{
		cfg:     cfg,
		logger:  cfg.Logger.With("exchange", "bitstamp"),
		manager: manager,
		readCh:  readCh,
		routes:  make(map[string]*bitstampChannelRoute),
	}, nil
}

// Name returns the exchange identifier.
func (b *Bitstamp) Name() string { return bitstampExchangeName }

// Start subscribes to configured feeds and begins streaming events.
func (b *Bitstamp) Start(parent context.Context, out chan<- *Event) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	var (
		forwardWG sync.WaitGroup
		internal  []*BitstampSubscription
	)

	for _, stream := range b.cfg.Streams {
		sub, err := b.Subscribe(ctx, stream)
		if err != nil {
			return fmt.Errorf("bitstamp subscribe %s: %w", stream, err)
		}
		internal = append(internal, sub)
		forwardWG.Go(func() {
			b.forwardSubscription(ctx, sub, out)
		})
	}

	errCh := make(chan error, 2)
	var runWG sync.WaitGroup
	runWG.Go(func() {
		errCh <- b.manager.Run(ctx)
	})

	runWG.Go(func() {
		errCh <- b.runReader(ctx)
	})

	var monitorWG sync.WaitGroup
	monitorWG.Go(func() {
		b.monitorConnection(ctx)
	})

	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-errCh:
	}

	cancel()
	runWG.Wait()
	monitorWG.Wait()

	for _, sub := range internal {
		_ = sub.Close()
	}
	forwardWG.Wait()

	if errors.Is(err, context.Canceled) {
		if parent.Err() != nil {
			return parent.Err()
		}
		return err
	}
	return err
}

// Subscribe registers interest in a qualified Bitstamp channel and returns a routed subscription handle.
func (b *Bitstamp) Subscribe(ctx context.Context, stream BitstampMarketChannel[Qualified]) (*BitstampSubscription, error) {
	if stream == "" {
		return nil, errors.New("bitstamp: stream is required")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}

	id, err := gonanoid.New(12)
	if err != nil {
		return nil, fmt.Errorf("bitstamp: create subscription id: %w", err)
	}

	sub := &BitstampSubscription{
		id:     id,
		stream: stream,
		ch:     make(chan *BitstampEvent, b.cfg.SubscriptionBuffer),
	}
	sub.closeFn = func() {
		b.dropSubscription(sub)
	}

	channelKey := stream.String()
	var firstSubscriber bool

	b.routesMu.Lock()
	route := b.routes[channelKey]
	if route == nil {
		route = &bitstampChannelRoute{stream: stream, subscribers: make(map[string]*BitstampSubscription)}
		b.routes[channelKey] = route
	}
	route.subscribers[sub.id] = sub
	firstSubscriber = len(route.subscribers) == 1
	b.routesMu.Unlock()

	if firstSubscriber && b.connected.Load() {
		if err := b.sendSubscribe(channelKey); err != nil {
			b.logger.Warn("bitstamp subscribe request failed", "channel", channelKey, "err", err)
		}
	}

	if ctx != nil && ctx.Done() != nil {
		go func() {
			<-ctx.Done()
			_ = sub.Close()
		}()
	}

	return sub, nil
}

func (b *Bitstamp) forwardSubscription(ctx context.Context, sub *BitstampSubscription, out chan<- *Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-sub.C():
			if !ok {
				return
			}
			payload := *evt
			select {
			case out <- &Event{Exchange: bitstampExchangeName, Stream: sub.Stream().String(), Payload: &payload, Time: evt.ReceivedAt}:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (b *Bitstamp) runReader(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-b.readCh:
			if !ok {
				return errors.New("bitstamp: read channel closed")
			}
			b.routeMessage(msg)
		}
	}
}

func (b *Bitstamp) routeMessage(msg socketMessage) {
	raw := json.RawMessage(msg.Data)
	if bitstampReconnectRequested(raw) {
		b.logger.Warn("bitstamp upstream requested reconnect")
		b.manager.ForceReconnect()
		return
	}

	channel, err := bitstampRouteKey(raw)
	if err != nil {
		b.logger.Warn("bitstamp route key missing", "err", err)
		return
	}
	if channel == "" {
		// Silently ignored message (e.g. subscription success)
		return
	}

	env, err := decodeBitstampEnvelope(raw)
	if err != nil {
		b.logger.Warn("bitstamp decode failed", "channel", channel, "err", err)
		return
	}

	subs := b.snapshotSubscribers(channel)
	if len(subs) == 0 {
		return
	}

	market := bitstampMarketFromChannel(env.Channel)
	for _, sub := range subs {
		evt := &BitstampEvent{
			SubscriptionID: sub.ID(),
			Stream:         sub.Stream(),
			Market:         market,
			Channel:        env.Channel,
			Event:          env.Event,
			Data:           env.Data,
			Raw:            raw,
			ReceivedAt:     msg.ReceivedAt,
		}
		if !sub.deliver(evt) {
			b.logger.Warn("bitstamp subscriber channel full, dropping message", "subscription_id", sub.ID(), "channel", env.Channel)
		}
	}
}

func (b *Bitstamp) snapshotSubscribers(channel string) []*BitstampSubscription {
	b.routesMu.RLock()
	defer b.routesMu.RUnlock()
	route := b.routes[channel]
	if route == nil || len(route.subscribers) == 0 {
		return nil
	}
	subs := make([]*BitstampSubscription, 0, len(route.subscribers))
	for _, sub := range route.subscribers {
		subs = append(subs, sub)
	}
	return subs
}

func (b *Bitstamp) monitorConnection(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-b.manager.Events():
			if !ok {
				return
			}
			switch evt.Type {
			case ConnectionEventConnected:
				b.connected.Store(true)
				b.resubscribeAll()
			case ConnectionEventDisconnected:
				b.connected.Store(false)
			}
		}
	}
}

func (b *Bitstamp) resubscribeAll() {
	streams := b.snapshotChannels()
	for _, stream := range streams {
		if err := b.sendSubscribe(stream.String()); err != nil {
			b.logger.Warn("bitstamp resubscribe failed", "channel", stream.String(), "err", err)
		}
	}
}

func (b *Bitstamp) snapshotChannels() []BitstampMarketChannel[Qualified] {
	b.routesMu.RLock()
	defer b.routesMu.RUnlock()
	streams := make([]BitstampMarketChannel[Qualified], 0, len(b.routes))
	for _, route := range b.routes {
		if len(route.subscribers) == 0 {
			continue
		}
		streams = append(streams, route.stream)
	}
	return streams
}

func (b *Bitstamp) dropSubscription(sub *BitstampSubscription) {
	channelKey := sub.Stream().String()
	var shouldUnsubscribe bool

	b.routesMu.Lock()
	if route, ok := b.routes[channelKey]; ok {
		delete(route.subscribers, sub.ID())
		if len(route.subscribers) == 0 {
			delete(b.routes, channelKey)
			shouldUnsubscribe = true
		}
	}
	b.routesMu.Unlock()

	sub.mu.Lock()
	if !sub.closed {
		close(sub.ch)
		sub.closed = true
	}
	sub.mu.Unlock()

	if shouldUnsubscribe && b.connected.Load() {
		if err := b.sendUnsubscribe(channelKey); err != nil {
			if errors.Is(err, ErrNotConnected) {
				return
			}
			b.logger.Warn("bitstamp unsubscribe failed", "channel", channelKey, "err", err)
		}
	}
}

func (b *Bitstamp) sendSubscribe(channel string) error {
	payload, err := bitstampSubscribePayload(channel)
	if err != nil {
		return err
	}
	return b.manager.Send(payload)
}

func (b *Bitstamp) sendUnsubscribe(channel string) error {
	payload, err := bitstampUnsubscribePayload(channel)
	if err != nil {
		return err
	}
	return b.manager.Send(payload)
}

// BitstampEvent represents the payload delivered over Event.Payload.
type BitstampEvent struct {
	SubscriptionID string
	Stream         BitstampMarketChannel[Qualified]
	Market         string
	Channel        string
	Event          string
	Data           json.RawMessage
	Raw            json.RawMessage
	ReceivedAt     time.Time
}

func (e *BitstampEvent) NormalizeTrade() (*Trade, error) {
	if e.Event != "trade" {
		return nil, fmt.Errorf("bitstamp: cannot normalize event type %q as trade", e.Event)
	}

	var data bitstampTradeData
	if err := json.Unmarshal(e.Data, &data); err != nil {
		return nil, fmt.Errorf("bitstamp: unmarshal trade data: %w", err)
	}

	side := "buy"
	if data.Type == 1 {
		side = "sell"
	}

	getString := func(v any) string {
		switch val := v.(type) {
		case string:
			return val
		case float64:
			return strconv.FormatFloat(val, 'f', -1, 64)
		default:
			return "0"
		}
	}

	// Prefer string fields if available to avoid precision loss
	price := data.PriceStr
	if price == "" {
		price = getString(data.Price)
	}
	amount := data.AmountStr
	if amount == "" {
		amount = getString(data.Amount)
	}
	id := data.IDStr
	if id == "" {
		id = strconv.FormatInt(data.ID, 10)
	}

	var timestamp time.Time
	if data.MicroTimestamp != "" {
		ts, err := strconv.ParseInt(data.MicroTimestamp, 10, 64)
		if err == nil {
			timestamp = time.UnixMicro(ts)
		}
	}
	if timestamp.IsZero() && data.Timestamp != "" {
		ts, err := strconv.ParseInt(data.Timestamp, 10, 64)
		if err == nil {
			timestamp = time.Unix(ts, 0)
		}
	}

	return &Trade{
		ID:        id,
		Exchange:  bitstampExchangeName,
		Market:    e.Market,
		Price:     price,
		Amount:    amount,
		Side:      side,
		Timestamp: timestamp,
	}, nil
}

type bitstampTradeData struct {
	ID             int64   `json:"id"`
	IDStr          string  `json:"id_str"`
	Amount         any     `json:"amount"`
	AmountStr      string  `json:"amount_str"`
	Price          any     `json:"price"`
	PriceStr       string  `json:"price_str"`
	Type           int     `json:"type"`
	Timestamp      string  `json:"timestamp"`
	MicroTimestamp string  `json:"microtimestamp"`
}

func bitstampSubscribePayload(channel string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"event": bitstampSubscribeEvent,
		"data":  map[string]string{"channel": channel},
	})
}

func bitstampUnsubscribePayload(channel string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"event": bitstampUnsubscribeEvent,
		"data":  map[string]string{"channel": channel},
	})
}

func bitstampRouteKey(raw []byte) (string, error) {
	var meta struct {
		Channel string `json:"channel"`
		Event   string `json:"event"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return "", err
	}
	// Ignore subscription success events which have no channel field in the top level object
	if meta.Event == "bts:subscription_succeeded" || meta.Event == "bts:heartbeat" {
		return "", nil // Handled silently
	}
	if meta.Channel == "" {
		return "", ErrBitstampRouteKeyMissing
	}
	return meta.Channel, nil
}

func bitstampReconnectRequested(raw []byte) bool {
	var meta struct {
		Event string `json:"event"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return false
	}
	return meta.Event == "bts:request_reconnect"
}

func decodeBitstampEnvelope(raw json.RawMessage) (*bitstampEnvelope, error) {
	env := &bitstampEnvelope{}
	if err := json.Unmarshal(raw, env); err != nil {
		return nil, err
	}
	return env, nil
}

func bitstampMarketFromChannel(channel string) string {
	idx := strings.LastIndex(channel, "_")
	if idx == -1 || idx+1 >= len(channel) {
		return ""
	}
	return channel[idx+1:]
}

type bitstampEnvelope struct {
	Event   string          `json:"event"`
	Channel string          `json:"channel"`
	Data    json.RawMessage `json:"data"`
}
