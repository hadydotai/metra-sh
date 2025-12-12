package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hadydotai/metra/exchanges"
	"hadydotai/metra/web"

	"github.com/a-h/templ"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	bitstamp, err := exchanges.NewBitstamp(
		exchanges.BitstampLogger(logger.With("exchange", "bitstamp")),
	)
	if err != nil {
		log.Fatalf("bitstamp init failed: %v", err)
	}

	binance, err := exchanges.NewBinance(
		exchanges.BinanceLogger(logger.With("exchange", "binance")),
	)
	if err != nil {
		log.Fatalf("binance init failed: %v", err)
	}

	// NOTE(@hadydotai): We're starting exchanges in background to maintain connections
	// and for now passing a discard sink until we start registering real subscriptions through the API.
	// Thing I keep thinking about is, how is this going to look like in 30, 60, 90 days from now. I need to monitor
	// this and keep an eye on connection health, also the logic for managing connection age is largely untested. So go
	// figure... Anyway.
	devnull := make(chan *exchanges.Event, 100)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-devnull:
				// discard
			}
		}
	}()

	go func() {
		if err := bitstamp.Start(ctx, devnull); err != nil && ctx.Err() == nil {
			logger.Error("bitstamp stopped", "err", err)
		}
	}()

	go func() {
		if err := binance.Start(ctx, devnull); err != nil && ctx.Err() == nil {
			logger.Error("binance stopped", "err", err)
		}
	}()

	mux := http.NewServeMux()

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))))

	// UI
	mux.Handle("GET /", templ.Handler(web.Home()))
	mux.Handle("GET /docs", templ.Handler(web.Docs()))

	// API Routes
	mux.HandleFunc("GET /stream/{exchange}/{pair}/{event}", func(w http.ResponseWriter, r *http.Request) {
		exchangeName := r.PathValue("exchange")
		pair := r.PathValue("pair")
		eventType := r.PathValue("event")

		var throttleDuration time.Duration
		if t := r.URL.Query().Get("throttle"); t != "" {
			var err error
			throttleDuration, err = time.ParseDuration(t)
			if err != nil {
				http.Error(w, `{"error": "invalid throttle duration"}`, http.StatusBadRequest)
				return
			}
		}

		// NOTE(@hadydotai): So we currently only support trades, spot trades to be exact. That's what we currently
		// normalize, probably should move this out of here when I add more
		if eventType != "trade" {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "requested event not found"})
			return
		}

		// NOTE(@hadydotai): Same story ^^^^
		if exchangeName != "binance" && exchangeName != "bitstamp" {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "requested exchange not found"})
			return
		}

		if !exchanges.ValidateMarket(exchangeName, pair) {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "requested pair not found"})
			return
		}

		var nextEvent func() (any, error)
		var closeFn func()

		switch exchangeName {
		case "bitstamp":
			qualified, _ := exchanges.BitstampLiveTickerChannel.WithMarket(pair)
			sub, err := bitstamp.Subscribe(r.Context(), qualified)
			if err != nil {
				logger.Error("bitstamp subscribe failed", "err", err)
				http.Error(w, `{"error": "subscription failed"}`, http.StatusInternalServerError)
				return
			}
			closeFn = func() { sub.Close() }

			nextEvent = func() (any, error) {
				select {
				case <-r.Context().Done():
					return nil, r.Context().Err()
				case evt, ok := <-sub.C():
					if !ok {
						return nil, fmt.Errorf("stream closed")
					}
					return evt, nil
				}
			}

		case "binance":
			qualified, _ := exchanges.BinanceTradeChannel.WithMarket(pair)
			sub, err := binance.Subscribe(r.Context(), qualified)
			if err != nil {
				logger.Error("binance subscribe failed", "err", err)
				http.Error(w, `{"error": "subscription failed"}`, http.StatusInternalServerError)
				return
			}
			closeFn = func() { sub.Close() }

			nextEvent = func() (any, error) {
				select {
				case <-r.Context().Done():
					return nil, r.Context().Err()
				case evt, ok := <-sub.C():
					if !ok {
						return nil, fmt.Errorf("stream closed")
					}
					return evt, nil
				}
			}
		}

		// Set SSE headers
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("X-Accel-Buffering", "no") // Disable proxy buffering (critical for Fly.io/Nginx)

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming not supported", http.StatusInternalServerError)
			return
		}

		defer closeFn()

		// Stream loop
		var lastSent time.Time
		for {
			evt, err := nextEvent()
			if err != nil {
				return
			}

			// Throttling logic: Drop message if we sent one too recently
			if throttleDuration > 0 && time.Since(lastSent) < throttleDuration {
				continue
			}

			var payload any = evt
			// Normalize if possible
			if binanceEvt, ok := evt.(*exchanges.BinanceEvent); ok {
				if normalized, err := binanceEvt.NormalizeTrade(); err == nil {
					payload = normalized
				}
			} else if bitstampEvt, ok := evt.(*exchanges.BitstampEvent); ok {
				if normalized, err := bitstampEvt.NormalizeTrade(); err == nil {
					payload = normalized
				}
			}

			// Normalize payload structure
			response := map[string]any{
				"event":    eventType,
				"exchange": exchangeName,
				"payload":  payload,
			}

			data, err := json.Marshal(response)
			if err != nil {
				logger.Error("json marshal failed", "err", err)
				continue
			}

			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			lastSent = time.Now()
		}
	})

	server := &http.Server{
		Addr:        ":8080",
		Handler:     securityMiddleware(mux),
		BaseContext: func(_ net.Listener) context.Context { return ctx },
	}

	go func() {
		logger.Info("server listening on :8080")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server failed: %v", err)
		}
	}()

	<-ctx.Done()
	// Shutdown gracefully, but don't block forever if handlers are stuck (though BaseContext fix should prevent stuck handlers)
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("server shutdown failed", "err", err)
	}
}

func securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// HSTS: Force HTTPS for 1 year, include subdomains
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains; preload")
		// XSS Protection
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Frame Options
		w.Header().Set("X-Frame-Options", "DENY")
		
		next.ServeHTTP(w, r)
	})
}
