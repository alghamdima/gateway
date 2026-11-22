package credits

import (
	"math"
	"testing"
)

func TestToMicroUSD(t *testing.T) {
	cases := []struct {
		name   string
		cost   float64
		markup int64
		want   int64
	}{
		{"zero", 0, 0, 0},
		{"one dollar", 1, 0, 1_000_000},
		{"a typical request", 0.000123, 0, 123},
		{"smaller than the unit rounds to zero", 0.0000004, 0, 0},
		{"just over half a unit rounds up", 0.00000051, 0, 1},
		{"just under half a unit rounds down", 0.00000049, 0, 0},
		{"5 percent markup", 1, 500, 1_050_000},
		{"5.5 percent markup", 2, 550, 2_110_000},
		{"markup is applied before the one rounding", 0.0000004, 3000, 1}, // 0.4 x 1.30 = 0.52 -> 1, while 0.4 alone -> 0
		{"large", 123_456.789012, 0, 123_456_789_012},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := toMicroUSD(c.cost, c.markup)
			if err != nil || got != c.want {
				t.Fatalf("toMicroUSD(%v, %d) = %d, %v; want %d", c.cost, c.markup, got, err, c.want)
			}
		})
	}
}

func TestToMicroUSD_RefusesWhatIsNotACost(t *testing.T) {
	for _, cost := range []float64{-0.01, math.NaN(), math.Inf(1), math.Inf(-1), 1e30} {
		if got, err := toMicroUSD(cost, 0); err == nil {
			t.Errorf("toMicroUSD(%v) = %d, want an error", cost, got)
		}
		if got, err := ceilMicroUSD(cost, 0); err == nil {
			t.Errorf("ceilMicroUSD(%v) = %d, want an error", cost, got)
		}
	}
	if _, err := toMicroUSD(1, -1); err == nil {
		t.Error("a negative markup must be an error")
	}
}

func TestCeilMicroUSD_NeverRoundsBelowTheAmount(t *testing.T) {
	cases := []struct {
		cost   float64
		markup int64
		want   int64
	}{
		{0, 0, 0},
		{1, 0, 1_000_000},
		{0.0000001, 0, 1},
		{0.0000010001, 0, 2},
		{0.000002, 0, 2},
		{1, 550, 1_055_000},
	}
	for _, c := range cases {
		got, err := ceilMicroUSD(c.cost, c.markup)
		if err != nil || got != c.want {
			t.Errorf("ceilMicroUSD(%v, %d) = %d, %v; want %d", c.cost, c.markup, got, err, c.want)
		}
		exact, _ := toMicroUSD(c.cost, c.markup)
		if got < exact {
			t.Errorf("ceilMicroUSD(%v) = %d is below the charge %d", c.cost, got, exact)
		}
	}
}

// A decimal amount like 0.0000005 USD has no exact float: the value actually stored is a hair
// below half a micro-USD. The conversion works on the stored value, so what can be promised is
// that the result is the nearest integer to it and that a larger cost never charges less.
func TestToMicroUSD_IsNearestAndMonotonic(t *testing.T) {
	var previous int64
	for i := 0; i <= 200_000; i++ {
		cost := float64(i) * 1.37e-8
		got, err := toMicroUSD(cost, 550)
		if err != nil {
			t.Fatal(err)
		}
		exact := cost * microPerUSD * 1.055
		if diff := math.Abs(float64(got) - exact); diff > 0.5+1e-6 {
			t.Fatalf("toMicroUSD(%v) = %d, more than half a unit from %v", cost, got, exact)
		}
		if got < previous {
			t.Fatalf("toMicroUSD(%v) = %d is below the charge for a smaller cost, %d", cost, got, previous)
		}
		previous = got
	}
}
