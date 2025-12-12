package exchanges

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var (
	ErrNotConnected = errors.New("exchanges: websocket connection is not established")
)

type backoffConfig struct {
	initial    time.Duration
	maxTries   time.Duration
	multiplier float64
	jitter     float64
}

type socketHeartBeat struct {
	interval time.Duration
	payload  []byte
}

// socketMessage carries the raw payload read from the websocket transport.
type socketMessage struct {
	Data       []byte
	ReceivedAt time.Time
}

// ConnectionEventType represents the lifecycle state of the websocket connection.
type ConnectionEventType int

const (
	ConnectionEventConnected ConnectionEventType = iota
	ConnectionEventDisconnected
)

// ConnectionEvent is emitted whenever the websocket connection transitions state.
type ConnectionEvent struct {
	Type ConnectionEventType
	Err  error
}

type connectionManagerConfig struct {
	wssURL string
	header http.Header

	heartbeat        *socketHeartBeat
	maxConnectionAge *time.Duration

	readCh       chan socketMessage
	readTimeout  time.Duration
	WriteTimeout time.Duration

	backoff backoffConfig
	logger  *slog.Logger
}

// connectionManager handles the nitty gritty details of websocket connection
// management, it takes care of heart beats, recycling connections past a certain age,
// and connection errors
type connectionManager struct {
	cfg connectionManagerConfig

	dialer *websocket.Dialer

	connMu sync.RWMutex
	conn   *websocket.Conn

	writeMu sync.Mutex

	events chan ConnectionEvent

	running atomic.Bool
}

func defaultConnectionManagerConfig() connectionManagerConfig {
	return connectionManagerConfig{
		header:       make(http.Header),
		readTimeout:  60 * time.Second,
		WriteTimeout: 2 * time.Second,
		backoff: backoffConfig{
			initial:    750 * time.Millisecond,
			maxTries:   2 * time.Minute,
			multiplier: 1.7,
			jitter:     0.4,
		},
		logger: slog.Default(),
	}
}

func newConnectionManager(wssURL string, opts connectionManagerConfig) (*connectionManager, error) {
	if wssURL == "" {
		return nil, errors.New("connection manager: websocket url is required")
	}

	cfg := defaultConnectionManagerConfig()
	cfg.wssURL = wssURL

	if opts.header != nil {
		cfg.header = opts.header
	}
	if opts.heartbeat != nil {
		cfg.heartbeat = opts.heartbeat
	}
	if opts.maxConnectionAge != nil {
		cfg.maxConnectionAge = opts.maxConnectionAge
	}
	if opts.readCh != nil {
		cfg.readCh = opts.readCh
	}
	if opts.readTimeout != 0 {
		cfg.readTimeout = opts.readTimeout
	}
	if opts.WriteTimeout != 0 {
		cfg.WriteTimeout = opts.WriteTimeout
	}
	if opts.backoff.initial != 0 {
		cfg.backoff.initial = opts.backoff.initial
	}
	if opts.backoff.maxTries != 0 {
		cfg.backoff.maxTries = opts.backoff.maxTries
	}
	if opts.backoff.multiplier != 0 {
		cfg.backoff.multiplier = opts.backoff.multiplier
	}
	if opts.backoff.jitter != 0 {
		cfg.backoff.jitter = opts.backoff.jitter
	}
	if opts.logger != nil {
		cfg.logger = opts.logger
	}

	if cfg.readCh == nil {
		return nil, errors.New("connection manager: read channel is required")
	}

	cm := &connectionManager{
		cfg:    cfg,
		events: make(chan ConnectionEvent, 8),
	}

	cm.dialer = &websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		HandshakeTimeout:  30 * time.Second,
		EnableCompression: true,
	}

	return cm, nil
}

// Events returns a read-only channel that emits connection lifecycle notifications.
func (cm *connectionManager) Events() <-chan ConnectionEvent {
	return cm.events
}

// Send transmits the provided payload over the active websocket connection.
func (cm *connectionManager) Send(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	conn := cm.currentConn()
	if conn == nil {
		return ErrNotConnected
	}
	return cm.write(conn, payload)
}

// ForceReconnect closes the current connection to initiate a fresh dial cycle.
func (cm *connectionManager) ForceReconnect() {
	cm.connMu.RLock()
	conn := cm.conn
	cm.connMu.RUnlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (cm *connectionManager) currentConn() *websocket.Conn {
	cm.connMu.RLock()
	defer cm.connMu.RUnlock()
	return cm.conn
}

func (cm *connectionManager) write(conn *websocket.Conn, payload []byte) error {
	if conn == nil {
		return ErrNotConnected
	}
	cm.writeMu.Lock()
	defer cm.writeMu.Unlock()
	if err := conn.SetWriteDeadline(time.Now().Add(cm.cfg.WriteTimeout)); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, payload)
}

func (cm *connectionManager) closeConn() {
	cm.connMu.Lock()
	conn := cm.conn
	cm.conn = nil
	cm.connMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (cm *connectionManager) publishEvent(evt ConnectionEvent) {
	select {
	case cm.events <- evt:
	default:
		cm.cfg.logger.Warn("connection event dropped", "event", evt.Type)
	}
}

// Run dials the websocket endpoint and keeps the transport alive until ctx is canceled.
func (cm *connectionManager) Run(ctx context.Context) error {
	if !cm.running.CompareAndSwap(false, true) {
		return errors.New("connection manager already running")
	}
	defer cm.running.Store(false)
	defer cm.closeConn()
	defer close(cm.events)

	attempt := 0

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		conn, resp, err := cm.dialer.DialContext(ctx, cm.cfg.wssURL, cm.cfg.header)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return ctx.Err()
			}
			delay := cm.nextBackoff(attempt)
			attempt++

			// NOTE(@hadydotai): Trying to figure out why we can't dial Binance on fly.io
			logAttrs := []any{"err", err, "retry_in", delay}
			if resp != nil {
				logAttrs = append(logAttrs, "status", resp.Status, "status_code", resp.StatusCode)
				// Log useful headers if available (e.g. rate limit headers)
				if val := resp.Header.Get("Retry-After"); val != "" {
					logAttrs = append(logAttrs, "header_retry_after", val)
				}
			}

			cm.cfg.logger.ErrorContext(ctx, "websocket dial error", logAttrs...)
			if waitErr := cm.wait(ctx, delay); waitErr != nil {
				return waitErr
			}
			continue
		}
		if resp != nil {
			resp.Body.Close()
		}

		attempt = 0
		_ = conn.SetReadDeadline(time.Now().Add(cm.cfg.readTimeout))
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(cm.cfg.readTimeout))
		})
		conn.SetPingHandler(func(message string) error {
			err := conn.WriteControl(websocket.PongMessage, []byte(message), time.Now().Add(cm.cfg.WriteTimeout))
			if err == nil {
				return conn.SetReadDeadline(time.Now().Add(cm.cfg.readTimeout))
			}
			return err
		})

		cm.connMu.Lock()
		cm.conn = conn
		cm.connMu.Unlock()
		cm.publishEvent(ConnectionEvent{Type: ConnectionEventConnected})

		connCtx, cancel := context.WithCancel(ctx)
		errCh := make(chan error, 1)

		go cm.readLoop(connCtx, conn, errCh)
		if cm.cfg.heartbeat != nil {
			go cm.heartBeatLoop(connCtx, conn, *cm.cfg.heartbeat, errCh)
		}
		if cm.cfg.maxConnectionAge != nil {
			go cm.connectionAgeLoop(connCtx, *cm.cfg.maxConnectionAge, errCh)
		}

		select {
		case <-ctx.Done():
			cancel()
			return ctx.Err()
		case err := <-errCh:
			cancel()
			cm.closeConn()
			cm.publishEvent(ConnectionEvent{Type: ConnectionEventDisconnected, Err: err})
			if errors.Is(err, context.Canceled) {
				return ctx.Err()
			}
			delay := cm.nextBackoff(attempt)
			attempt++
			cm.cfg.logger.ErrorContext(ctx, "websocket connection error", "err", err, "retry_in", delay)
			if waitErr := cm.wait(ctx, delay); waitErr != nil {
				return waitErr
			}
		}
	}
}

func (cm *connectionManager) heartBeatLoop(ctx context.Context, conn *websocket.Conn, cfg socketHeartBeat, errCh chan<- error) {
	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := cm.write(conn, cfg.payload); err != nil {
				errCh <- err
				return
			}
		}
	}
}

func (cm *connectionManager) readLoop(ctx context.Context, conn *websocket.Conn, errCh chan<- error) {
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		if err := conn.SetReadDeadline(time.Now().Add(cm.cfg.readTimeout)); err != nil {
			errCh <- err
			return
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			errCh <- err
			return
		}
		msg := socketMessage{Data: data, ReceivedAt: time.Now().UTC()}
		select {
		case cm.cfg.readCh <- msg:
		case <-ctx.Done():
			return
		}
	}
}

func (cm *connectionManager) connectionAgeLoop(ctx context.Context, maxConnectionAge time.Duration, errCh chan<- error) {
	timer := time.NewTimer(maxConnectionAge)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
		errCh <- fmt.Errorf("connection max age %.0f hours reached", maxConnectionAge.Hours())
	}
}

func (cm *connectionManager) wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (cm *connectionManager) nextBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		return cm.cfg.backoff.initial
	}
	expo := float64(cm.cfg.backoff.initial) * math.Pow(cm.cfg.backoff.multiplier, float64(attempt))
	if max := float64(cm.cfg.backoff.maxTries); max > 0 && expo > max {
		expo = max
	}
	jitter := cm.cfg.backoff.jitter
	if jitter > 0 {
		spread := 1 + (rand.Float64()*2-1)*jitter
		expo *= spread
	}
	if expo < float64(cm.cfg.backoff.initial) {
		expo = float64(cm.cfg.backoff.initial)
	}
	return time.Duration(expo)
}
