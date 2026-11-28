package credits

import (
	"math"
	"testing"
)

// A charge is the provider's cost at the effective rate and nothing more: no markup.
func TestToMicroSAR(t *testing.T) {
	cases := []struct {
		name string
		cost float64
		rate int64
		want int64
	}{
		{"zero", 0, pegRate, 0},
		{"one dollar at the peg", 1, pegRate, 3_750_000},
		{"one dollar at parity", 1, atParity, 1_000_000},
		{"one dollar at the peg with a 2.5 percent spread", 1, 3_843_750, 3_843_750},
		{"a typical request", 0.000123, pegRate, 461}, // 461.25
		{"a rate with six decimals", 1, 3_751_234, 3_751_234},
		{"a fraction of a micro-SAR rounds half up", 0.0000002, pegRate, 1}, // 0.75
		{"under half a micro-SAR rounds down", 0.0000001, pegRate, 0},       // 0.375
		{"smaller than the unit rounds to zero", 0.0000004, atParity, 0},
		{"just over half a unit rounds up", 0.00000051, atParity, 1},
		{"just under half a unit rounds down", 0.00000049, atParity, 0},
		// 0.44 x 3.75 = 1.65 -> 2, while converting a rounded micro-USD (0) would charge nothing
		{"the rate is applied before the one rounding", 0.00000044, pegRate, 2},
		{"large", 123_456.789012, pegRate, 462_962_958_795}, // 462,962,958,795.0
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := toMicroSAR(c.cost, c.rate)
			if err != nil || got != c.want {
				t.Fatalf("toMicroSAR(%v, %d) = %d, %v; want %d", c.cost, c.rate, got, err, c.want)
			}
		})
	}
}

func TestMoney_RefusesWhatIsNotACostOrARate(t *testing.T) {
	for _, cost := range []float64{-0.01, math.NaN(), math.Inf(1), math.Inf(-1), 1e30} {
		if got, err := toMicroSAR(cost, pegRate); err == nil {
			t.Errorf("toMicroSAR(%v) = %d, want an error", cost, got)
		}
		if got, err := ceilMicroSAR(cost, pegRate); err == nil {
			t.Errorf("ceilMicroSAR(%v) = %d, want an error", cost, got)
		}
		if got, err := toMicroUSD(cost); err == nil {
			t.Errorf("toMicroUSD(%v) = %d, want an error", cost, got)
		}
	}
	// a missing rate must never turn a cost into a free request
	for _, rate := range []int64{0, -3_750_000} {
		if got, err := toMicroSAR(1, rate); err == nil {
			t.Errorf("toMicroSAR at rate %d = %d, want an error", rate, got)
		}
		if got, err := ceilMicroSAR(1, rate); err == nil {
			t.Errorf("ceilMicroSAR at rate %d = %d, want an error", rate, got)
		}
	}
}

func TestCeilMicroSAR_NeverRoundsBelowTheAmount(t *testing.T) {
	cases := []struct {
		cost float64
		rate int64
		want int64
	}{
		{0, pegRate, 0},
		{1, pegRate, 3_750_000},
		{0.0000001, pegRate, 1}, // 0.375 is held as 1
		{0.0000001, atParity, 1},
		{0.0000010001, atParity, 2},
		{0.000002, atParity, 2},
		{0.000002, pegRate, 8},     // 7.5
		{0.000002, 3_843_750, 8},   // 7.6875
		{0.00045, pegRate, 1688},   // 1687.5
		{0.00045, 3_843_750, 1730}, // 1729.6875
		{0.000123, 3_843_750, 473}, // 472.78125
	}
	for _, c := range cases {
		got, err := ceilMicroSAR(c.cost, c.rate)
		if err != nil || got != c.want {
			t.Errorf("ceilMicroSAR(%v, %d) = %d, %v; want %d", c.cost, c.rate, got, err, c.want)
		}
		exact, _ := toMicroSAR(c.cost, c.rate)
		if got < exact {
			t.Errorf("ceilMicroSAR(%v) = %d is below the charge %d", c.cost, got, exact)
		}
	}
}

func TestToMicroUSD_IsTheProviderCost(t *testing.T) {
	cases := []struct {
		cost float64
		want int64
	}{
		{0, 0},
		{1, 1_000_000},
		{0.000123, 123},
		{0.00000051, 1},
		{0.00000049, 0},
	}
	for _, c := range cases {
		if got, err := toMicroUSD(c.cost); err != nil || got != c.want {
			t.Errorf("toMicroUSD(%v) = %d, %v; want %d", c.cost, got, err, c.want)
		}
	}
}

// A decimal amount like 0.0000005 USD has no exact float: the value actually stored is a hair
// below half a micro-unit. The conversion works on the stored value, so what can be promised is
// that the result is the nearest integer to cost x rate, that a larger cost never charges less,
// and that the hold for a cost is never below its charge and at most one micro-SAR above it.
func TestToMicroSAR_IsNearestAndMonotonic(t *testing.T) {
	const effective = int64(3_843_750) // the peg with the bank's 2.5 percent
	var previous int64
	for i := 0; i <= 200_000; i++ {
		cost := float64(i) * 1.37e-8
		got, err := toMicroSAR(cost, effective)
		if err != nil {
			t.Fatal(err)
		}
		exact := cost * microPerUnit * 3.84375
		if diff := math.Abs(float64(got) - exact); diff > 0.5+1e-6 {
			t.Fatalf("toMicroSAR(%v) = %d, more than half a unit from %v", cost, got, exact)
		}
		if got < previous {
			t.Fatalf("toMicroSAR(%v) = %d is below the charge for a smaller cost, %d", cost, got, previous)
		}
		if held, _ := ceilMicroSAR(cost, effective); held < got || held > got+1 {
			t.Fatalf("ceilMicroSAR(%v) = %d for a charge of %d", cost, held, got)
		}
		previous = got
	}
}
