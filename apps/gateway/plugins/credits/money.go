package credits

import (
	"fmt"
	"math"
	"math/big"
)

// The ledger is in micro-SAR, the customer's currency (docs/platform/adr/0017-sar-ledger.md):
// 1 SAR = 1,000,000 micro-SAR. Provider costs are USD floats from the model catalog. A cost
// is converted once per attempt, at the effective USD to SAR rate: the official rate staff
// set, stored x 1,000,000 as an exact integer (3.75 is 3,750,000), plus the bank's spread
// (fxRate). With "rate e6" the effective rate:
//
//	micro-SAR = cost USD x 1,000,000 x (rate e6 / 1,000,000) = cost USD x rate e6
//
// There is no markup: a request is charged its provider's cost, and the platform's revenue
// is the fee taken at top-up (docs/platform/adr/0010-admin-controlled-pricing.md).
//
// The arithmetic is exact on the value the float actually holds and never multiplies as a
// float, so the only rounding is the final one and the result is the same on every machine.
// A hold rounds up, so it is never smaller than what it stands for; a charge rounds half up
// to the nearest micro-SAR. Both use the same rate: the hold's.

// fxRate is how USD becomes SAR for one attempt: the official rate x 1,000,000, the bank's
// conversion commission in basis points, and the effective rate both give,
// ceil(official x (10,000 + spread) / 10,000), rounded up so the commission is never
// under-counted. The hold, its reservation row and its usage entry all store the three.
type fxRate struct {
	officialE6  int64
	spreadBps   int64
	effectiveE6 int64
}

// newFXRate computes the effective rate. An official rate that is not positive and a
// negative spread are errors: a missing rate must never make a request free.
func newFXRate(officialE6, spreadBps int64) (fxRate, error) {
	if officialE6 <= 0 {
		return fxRate{}, fmt.Errorf("credits: USD to SAR rate %d (x 1,000,000) is not positive", officialE6)
	}
	if spreadBps < 0 {
		return fxRate{}, fmt.Errorf("credits: negative FX spread %d bps", spreadBps)
	}
	product := new(big.Int).Mul(big.NewInt(officialE6), big.NewInt(10_000+spreadBps))
	quo, rem := new(big.Int).QuoRem(product, big.NewInt(10_000), new(big.Int))
	if rem.Sign() != 0 {
		quo.Add(quo, big.NewInt(1))
	}
	if !quo.IsInt64() || quo.Int64() > math.MaxInt32 {
		return fxRate{}, fmt.Errorf("credits: USD to SAR rate %d with %d bps does not fit", officialE6, spreadBps)
	}
	return fxRate{officialE6: officialE6, spreadBps: spreadBps, effectiveE6: quo.Int64()}, nil
}

// microPerUnit is the ledger's scale: 1 SAR (or 1 USD) = 1,000,000 micro-units.
const microPerUnit = 1_000_000

var ratHalf = big.NewRat(1, 2)

// exactMicro is cost x rate as an exact rational, in micro-units of the target currency.
// rateE6 is the rate x 1,000,000. A negative, NaN or infinite cost and a rate that is not
// positive are errors, never a credit.
func exactMicro(costUSD float64, rateE6 int64) (*big.Rat, error) {
	if math.IsNaN(costUSD) || math.IsInf(costUSD, 0) || costUSD < 0 {
		return nil, fmt.Errorf("credits: invalid cost %v", costUSD)
	}
	if rateE6 <= 0 {
		return nil, fmt.Errorf("credits: invalid USD to SAR rate %d (x 1,000,000)", rateE6)
	}
	amount := new(big.Rat).SetFloat64(costUSD)
	// micro-units x rate: the 1,000,000 of the unit and the 1,000,000 of the rate cancel
	amount.Mul(amount, big.NewRat(rateE6, 1))
	return amount, nil
}

func toInt64(value *big.Int, costUSD float64) (int64, error) {
	if !value.IsInt64() {
		return 0, fmt.Errorf("credits: cost %v does not fit the ledger", costUSD)
	}
	return value.Int64(), nil
}

// toMicroSAR is what an attempt that cost costUSD is charged, in micro-SAR, at the rate of
// its hold: rounded once to the nearest micro-SAR, half up.
func toMicroSAR(costUSD float64, rateE6 int64) (int64, error) {
	amount, err := exactMicro(costUSD, rateE6)
	if err != nil {
		return 0, err
	}
	amount.Add(amount, ratHalf)
	// floor(x + 1/2) is round half up for a non-negative x
	return toInt64(new(big.Int).Quo(amount.Num(), amount.Denom()), costUSD)
}

// ceilMicroSAR is toMicroSAR rounded up instead of half up. An estimate that will be held
// against a balance must never be smaller than the amount it stands for.
func ceilMicroSAR(costUSD float64, rateE6 int64) (int64, error) {
	amount, err := exactMicro(costUSD, rateE6)
	if err != nil {
		return 0, err
	}
	quo, rem := new(big.Int).QuoRem(amount.Num(), amount.Denom(), new(big.Int))
	if rem.Sign() != 0 {
		quo.Add(quo, big.NewInt(1))
	}
	return toInt64(quo, costUSD)
}

// toMicroUSD is the provider's cost in micro-USD, rounded half up: what a
// usage entry stores beside its SAR amount, for reconciliation against the gateway's log.
// The charge is converted from the cost itself, not from this rounded figure.
func toMicroUSD(costUSD float64) (int64, error) {
	return toMicroSAR(costUSD, microPerUnit)
}
