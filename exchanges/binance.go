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
	binanceExchangeName = "binance"
	binanceDefaultWSURL = "wss://stream.binance.com:9443/ws"
)

var (
	ErrBinanceUnsupportedMarket = errors.New("binance: subscription not found")
	ErrBinanceRouteKeyMissing   = errors.New("binance: route key missing")
)

type BinanceMarketChannel[T qualificationStage] string

const (
	BinanceTradeChannel BinanceMarketChannel[Unqualified] = "trade"
)

func (channel BinanceMarketChannel[Unqualified]) WithMarket(market string) (BinanceMarketChannel[Qualified], error) {
	market = strings.ToLower(market)
	if _, ok := binanceMarkets[market]; !ok {
		return "", fmt.Errorf("unknown binance market %q: %w", market, ErrBinanceUnsupportedMarket)
	}
	return BinanceMarketChannel[Qualified](fmt.Sprintf("%s@%s", market, string(channel))), nil
}

func (channel BinanceMarketChannel[Qualified]) String() string {
	return string(channel)
}

type BinanceConfig struct {
	WebsocketURL       string
	Streams            []BinanceMarketChannel[Qualified]
	SubscriptionBuffer int
	Logger             *slog.Logger
}

func binanceDefaultConfig() BinanceConfig {
	return BinanceConfig{
		WebsocketURL:       binanceDefaultWSURL,
		SubscriptionBuffer: 256,
		Logger:             slog.Default(),
	}
}

func BinanceWebsocketURL(wssURL string) ConfigFunc[BinanceConfig] {
	return func(bc *BinanceConfig) *BinanceConfig {
		bc.WebsocketURL = wssURL
		return bc
	}
}

func BinanceSubscribe(stream BinanceMarketChannel[Qualified]) ConfigFunc[BinanceConfig] {
	return func(bc *BinanceConfig) *BinanceConfig {
		bc.Streams = append(bc.Streams, stream)
		return bc
	}
}

func BinanceSubscriptionBuffer(size int) ConfigFunc[BinanceConfig] {
	return func(bc *BinanceConfig) *BinanceConfig {
		bc.SubscriptionBuffer = size
		return bc
	}
}

func BinanceLogger(logger *slog.Logger) ConfigFunc[BinanceConfig] {
	return func(bc *BinanceConfig) *BinanceConfig {
		bc.Logger = logger
		return bc
	}
}

type BinanceSubscription struct {
	id     string
	stream BinanceMarketChannel[Qualified]
	ch     chan *BinanceEvent

	closeOnce sync.Once
	closeFn   func()

	mu     sync.RWMutex
	closed bool
}

func (s *BinanceSubscription) ID() string { return s.id }

func (s *BinanceSubscription) Stream() BinanceMarketChannel[Qualified] { return s.stream }

func (s *BinanceSubscription) C() <-chan *BinanceEvent { return s.ch }

func (s *BinanceSubscription) Close() error {
	s.closeOnce.Do(func() {
		if s.closeFn != nil {
			s.closeFn()
		}
	})
	return nil
}

func (s *BinanceSubscription) deliver(evt *BinanceEvent) bool {
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

type binanceChannelRoute struct {
	stream      BinanceMarketChannel[Qualified]
	subscribers map[string]*BinanceSubscription
}

type Binance struct {
	cfg    BinanceConfig
	logger *slog.Logger

	manager *connectionManager
	readCh  chan socketMessage

	routesMu sync.RWMutex
	routes   map[string]*binanceChannelRoute

	connected atomic.Bool
}

func NewBinance(cfgFns ...ConfigFunc[BinanceConfig]) (*Binance, error) {
	cfg := binanceDefaultConfig()
	for _, fn := range cfgFns {
		fn(&cfg)
	}

	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.SubscriptionBuffer <= 0 {
		cfg.SubscriptionBuffer = 256
	}

	maxConnectionAge := 24 * time.Hour // Binance connections last 24h usually
	readBuffer := max(cfg.SubscriptionBuffer*4, 1024)
	readCh := make(chan socketMessage, readBuffer)

	opts := connectionManagerConfig{
		readCh:           readCh,
		maxConnectionAge: &maxConnectionAge,

		// Binance doesn't use app level heartbeats, uses Ping/Pong frames.
		// So we'll leave this nil
		heartbeat: nil,

		readTimeout: 60 * time.Second,
		// Binance pings us with a PING frame every 20 seconds, we'll Pong immediately with a 10s timeout
		// this is handled by the ping handler on the connection manager, so no need to worry about it here but we can't
		// wait beyond 10s, if our peer is congested we'll simply cycle with a write timeout error on the CM
		WriteTimeout: 10 * time.Second,
		backoff: backoffConfig{
			initial:    750 * time.Millisecond,
			maxTries:   2 * time.Minute,
			multiplier: 1.7,
			jitter:     0.4,
		},
		logger: cfg.Logger.With("exchange", "binance"),
	}

	manager, err := newConnectionManager(cfg.WebsocketURL, opts)
	if err != nil {
		return nil, err
	}

	return &Binance{
		cfg:     cfg,
		logger:  cfg.Logger.With("exchange", "binance"),
		manager: manager,
		readCh:  readCh,
		routes:  make(map[string]*binanceChannelRoute),
	}, nil
}

func (b *Binance) Name() string { return binanceExchangeName }

func (b *Binance) Start(parent context.Context, out chan<- *Event) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	var (
		forwardWG sync.WaitGroup
		internal  []*BinanceSubscription
	)

	for _, stream := range b.cfg.Streams {
		sub, err := b.Subscribe(ctx, stream)
		if err != nil {
			return fmt.Errorf("binance subscribe %s: %w", stream, err)
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

func (b *Binance) Subscribe(ctx context.Context, stream BinanceMarketChannel[Qualified]) (*BinanceSubscription, error) {
	if stream == "" {
		return nil, errors.New("binance: stream is required")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}

	id, err := gonanoid.New(12)
	if err != nil {
		return nil, fmt.Errorf("binance: create subscription id: %w", err)
	}

	sub := &BinanceSubscription{
		id:     id,
		stream: stream,
		ch:     make(chan *BinanceEvent, b.cfg.SubscriptionBuffer),
	}
	sub.closeFn = func() {
		b.dropSubscription(sub)
	}

	channelKey := stream.String()
	var firstSubscriber bool

	b.routesMu.Lock()
	route := b.routes[channelKey]
	if route == nil {
		route = &binanceChannelRoute{stream: stream, subscribers: make(map[string]*BinanceSubscription)}
		b.routes[channelKey] = route
	}
	route.subscribers[sub.id] = sub
	firstSubscriber = len(route.subscribers) == 1
	b.routesMu.Unlock()

	if firstSubscriber && b.connected.Load() {
		if err := b.sendSubscribe(channelKey); err != nil {
			b.logger.Warn("binance subscribe request failed", "channel", channelKey, "err", err)
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

func (b *Binance) forwardSubscription(ctx context.Context, sub *BinanceSubscription, out chan<- *Event) {
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
			case out <- &Event{Exchange: binanceExchangeName, Stream: sub.Stream().String(), Payload: &payload, Time: evt.ReceivedAt}:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (b *Binance) runReader(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-b.readCh:
			if !ok {
				return errors.New("binance: read channel closed")
			}
			b.routeMessage(msg)
		}
	}
}

func (b *Binance) routeMessage(msg socketMessage) {
	raw := json.RawMessage(msg.Data)

	if binanceIsControlMessage(raw) {
		return
	}

	channel, err := binanceRouteKey(raw)
	if err != nil {
		b.logger.Warn("binance route key missing", "err", err, "data", string(raw))
		return
	}

	subs := b.snapshotSubscribers(channel)
	if len(subs) == 0 {
		return
	}

	var trade binanceTradeEvent
	if err := json.Unmarshal(raw, &trade); err != nil {
		b.logger.Warn("binance trade decode failed", "channel", channel, "err", err)
		return
	}

	for _, sub := range subs {
		evt := &BinanceEvent{
			SubscriptionID: sub.ID(),
			Stream:         sub.Stream(),
			Symbol:         trade.Symbol,
			EventType:      trade.EventType,
			EventTime:      time.UnixMilli(trade.EventTime),
			TradeTime:      time.UnixMilli(trade.TradeTime),
			Price:          trade.Price,
			Quantity:       trade.Quantity,
			BuyerOrderID:   trade.BuyerOrderID,
			SellerOrderID:  trade.SellerOrderID,
			TradeID:        trade.TradeID,
			IsMaker:        trade.IsBuyerMaker,
			Raw:            raw,
			ReceivedAt:     msg.ReceivedAt,
		}
		if !sub.deliver(evt) {
			b.logger.Warn("binance subscriber channel full, dropping message", "subscription_id", sub.ID(), "channel", channel)
		}
	}
}

func (b *Binance) snapshotSubscribers(channel string) []*BinanceSubscription {
	b.routesMu.RLock()
	defer b.routesMu.RUnlock()
	route := b.routes[channel]
	if route == nil || len(route.subscribers) == 0 {
		return nil
	}
	subs := make([]*BinanceSubscription, 0, len(route.subscribers))
	for _, sub := range route.subscribers {
		subs = append(subs, sub)
	}
	return subs
}

func (b *Binance) monitorConnection(ctx context.Context) {
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

func (b *Binance) resubscribeAll() {
	streams := b.snapshotChannels()
	// NOTE(@hadydotai): Binance allows multiple streams in one subscribe, but we can do one by one for simplicity
	// and later batch them. The API limit is generous.
	for _, stream := range streams {
		if err := b.sendSubscribe(stream.String()); err != nil {
			b.logger.Warn("binance resubscribe failed", "channel", stream.String(), "err", err)
		}
	}
}

func (b *Binance) snapshotChannels() []BinanceMarketChannel[Qualified] {
	b.routesMu.RLock()
	defer b.routesMu.RUnlock()
	streams := make([]BinanceMarketChannel[Qualified], 0, len(b.routes))
	for _, route := range b.routes {
		if len(route.subscribers) == 0 {
			continue
		}
		streams = append(streams, route.stream)
	}
	return streams
}

func (b *Binance) dropSubscription(sub *BinanceSubscription) {
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
			b.logger.Warn("binance unsubscribe failed", "channel", channelKey, "err", err)
		}
	}
}

func (b *Binance) sendSubscribe(channel string) error {
	payload, err := binanceSubscribePayload(channel)
	if err != nil {
		return err
	}
	return b.manager.Send(payload)
}

func (b *Binance) sendUnsubscribe(channel string) error {
	payload, err := binanceUnsubscribePayload(channel)
	if err != nil {
		return err
	}
	return b.manager.Send(payload)
}

type BinanceEvent struct {
	SubscriptionID string
	Stream         BinanceMarketChannel[Qualified]
	Symbol         string
	EventType      string
	EventTime      time.Time
	TradeTime      time.Time
	Price          string
	Quantity       string
	BuyerOrderID   int64
	SellerOrderID  int64
	TradeID        int64
	IsMaker        bool
	Raw            json.RawMessage
	ReceivedAt     time.Time
}

func (e *BinanceEvent) NormalizeTrade() (*Trade, error) {
	side := "buy"
	if e.IsMaker {
		side = "sell"
	}
	return &Trade{
		ID:        strconv.FormatInt(e.TradeID, 10),
		Exchange:  binanceExchangeName,
		Market:    e.Symbol,
		Price:     e.Price,
		Amount:    e.Quantity,
		Side:      side,
		Timestamp: e.TradeTime,
	}, nil
}

func binanceSubscribePayload(channel string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"method": "SUBSCRIBE",
		"params": []string{channel},
		"id":     time.Now().UnixNano(),
	})
}

func binanceUnsubscribePayload(channel string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"method": "UNSUBSCRIBE",
		"params": []string{channel},
		"id":     time.Now().UnixNano(),
	})
}

func binanceRouteKey(raw []byte) (string, error) {
	var meta struct {
		EventType string `json:"e"`
		EventTime int64  `json:"E"`
		Symbol    string `json:"s"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return "", err
	}
	if meta.EventType == "" || meta.Symbol == "" {
		return "", ErrBinanceRouteKeyMissing
	}
	// Reconstruct channel: symbol@eventType
	return fmt.Sprintf("%s@%s", strings.ToLower(meta.Symbol), meta.EventType), nil
}

func binanceIsControlMessage(raw []byte) bool {
	var meta struct {
		Result any `json:"result"`
		ID     any `json:"id"`
	}
	// NOTE(@hadydotai): If it has "result" (even null) and "id", it's a control response, I think...
	if err := json.Unmarshal(raw, &meta); err == nil {
		return meta.ID != nil
	}
	return false
}

type binanceTradeEvent struct {
	EventType     string `json:"e"`
	EventTime     int64  `json:"E"`
	Symbol        string `json:"s"`
	TradeID       int64  `json:"t"`
	Price         string `json:"p"`
	Quantity      string `json:"q"`
	BuyerOrderID  int64  `json:"b"`
	SellerOrderID int64  `json:"a"`
	TradeTime     int64  `json:"T"`
	IsBuyerMaker  bool   `json:"m"`
	Ignore        bool   `json:"M"`
}
