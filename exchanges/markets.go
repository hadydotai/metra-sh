package exchanges

import (
	"sort"
	"strings"
)

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

func GetSupportedPairs(exchange string) []string {
	var pairs []string
	switch strings.ToLower(exchange) {
	case "bitstamp":
		pairs = make([]string, 0, len(bitstampMarkets))
		for p := range bitstampMarkets {
			pairs = append(pairs, p)
		}
	case "binance":
		pairs = make([]string, 0, len(binanceMarkets))
		for p := range binanceMarkets {
			pairs = append(pairs, p)
		}
	}
	sort.Strings(pairs)
	return pairs
}
