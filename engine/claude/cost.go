package claude

import (
	"math/big"
	"strings"

	"github.com/alaindgonz-cell/intellectus/engine/llm"
)

// price is USD per million tokens, as exact decimal strings.
type price struct {
	input, output, cacheRead string
}

// prices are list prices from the claude-api skill reference (Sept 2026).
// Cache writes are billed at 1.25x input (5-minute TTL, the TTL this client
// uses). Cache reads are 0.1x input except where the reference lists a
// model-specific rate: $0.20/MTok on claude-opus-5-5 and $0.25/MTok on
// claude-fable-5-1.
var prices = map[string]price{
	"claude-opus-5":             {"5", "25", "0.5"},
	"claude-opus-5-5":           {"4", "20", "0.2"},
	"claude-sonnet-5":           {"2", "10", "0.2"},
	"claude-fable-5-1":          {"10", "50", "0.25"},
	"claude-haiku-4-5":          {"1", "5", "0.1"},
	"claude-haiku-4-5-20251001": {"1", "5", "0.1"},
}

// EstimateCostUSD returns an ESTIMATE of the list-price cost of usage on
// model, as an exact decimal string of US dollars (e.g. "0.0415"), and false
// for a model without a known price. It ignores batch/priority discounts,
// fast mode, 1-hour cache writes, and — after a server-side fallback — the
// fact that the fallback attempt bills at the fallback model's rates.
func EstimateCostUSD(model string, u llm.Usage) (string, bool) {
	p, ok := prices[model]
	if !ok {
		return "", false
	}
	rat := func(s string) *big.Rat {
		r, _ := new(big.Rat).SetString(s)
		return r
	}
	tokens := func(n int64) *big.Rat { return new(big.Rat).SetInt64(n) }
	in := rat(p.input)
	write := new(big.Rat).Mul(in, rat("1.25"))
	total := new(big.Rat)
	total.Add(total, new(big.Rat).Mul(tokens(u.InputTokens), in))
	total.Add(total, new(big.Rat).Mul(tokens(u.OutputTokens), rat(p.output)))
	total.Add(total, new(big.Rat).Mul(tokens(u.CacheCreationInputTokens), write))
	total.Add(total, new(big.Rat).Mul(tokens(u.CacheReadInputTokens), rat(p.cacheRead)))
	total.Quo(total, rat("1000000"))
	return decimal(total), true
}

// decimal renders r exactly (it has a terminating expansion: every price
// has at most 4 decimals and we divide by 10^6), trimming trailing zeros.
func decimal(r *big.Rat) string {
	s := r.FloatString(12)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	if s == "-0" {
		s = "0"
	}
	return s
}
