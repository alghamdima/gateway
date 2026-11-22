package credits

import (
	"fmt"
	"math"
	"math/big"
)

// microPerUSD is the ledger's unit: 1 USD = 1,000,000 micro-USD.
const microPerUSD = 1_000_000

var (
	ratMicro = big.NewRat(microPerUSD, 1)
	ratHalf  = big.NewRat(1, 2)
)

// toMicroUSD converts a provider cost in USD to what the customer is charged in micro-USD:
// cost x (10000 + markupBps) / 10000, rounded once to the nearest unit (half up). The
// arithmetic is exact on the value the float actually holds and never multiplies as a float,
// so the only rounding is the final one and the result is the same on every machine.
// A negative, NaN or infinite cost is an error, never a credit.
func toMicroUSD(costUSD float64, markupBps int64) (int64, error) {
	if math.IsNaN(costUSD) || math.IsInf(costUSD, 0) || costUSD < 0 {
		return 0, fmt.Errorf("credits: invalid cost %v", costUSD)
	}
	if markupBps < 0 {
		return 0, fmt.Errorf("credits: invalid markup %d bps", markupBps)
	}
	if costUSD == 0 {
		return 0, nil
	}
	amount := new(big.Rat).SetFloat64(costUSD)
	amount.Mul(amount, ratMicro)
	amount.Mul(amount, big.NewRat(10_000+markupBps, 10_000))
	amount.Add(amount, ratHalf)
	// floor(x + 1/2) is round half up for a non-negative x
	rounded := new(big.Int).Quo(amount.Num(), amount.Denom())
	if !rounded.IsInt64() {
		return 0, fmt.Errorf("credits: cost %v does not fit the ledger", costUSD)
	}
	return rounded.Int64(), nil
}

// ceilMicroUSD is toMicroUSD rounded up instead of half up. An estimate that will be held
// against a balance must never be smaller than the amount it stands for.
func ceilMicroUSD(costUSD float64, markupBps int64) (int64, error) {
	if math.IsNaN(costUSD) || math.IsInf(costUSD, 0) || costUSD < 0 {
		return 0, fmt.Errorf("credits: invalid cost %v", costUSD)
	}
	if markupBps < 0 {
		return 0, fmt.Errorf("credits: invalid markup %d bps", markupBps)
	}
	if costUSD == 0 {
		return 0, nil
	}
	amount := new(big.Rat).SetFloat64(costUSD)
	amount.Mul(amount, ratMicro)
	amount.Mul(amount, big.NewRat(10_000+markupBps, 10_000))
	quo, rem := new(big.Int).QuoRem(amount.Num(), amount.Denom(), new(big.Int))
	if rem.Sign() != 0 {
		quo.Add(quo, big.NewInt(1))
	}
	if !quo.IsInt64() {
		return 0, fmt.Errorf("credits: cost %v does not fit the ledger", costUSD)
	}
	return quo.Int64(), nil
}
