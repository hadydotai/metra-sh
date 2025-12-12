package exchanges

import "strings"

func ValidateMarket(exchange, pair string) bool {
	pair = strings.ToLower(pair)
	switch strings.ToLower(exchange) {
	case "bitstamp":
		_, ok := bitstampMarkets[pair]
		return ok
	case "binance":
		_, ok := binanceMarkets[pair]
		return ok
	default:
		return false
	}
}
