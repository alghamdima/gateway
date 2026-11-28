package credits

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// The ledger is in micro-SAR; provider costs are USD (docs/platform/adr/0017-sar-ledger.md).
// testPricer charges 0.002 USD for 1000 input and 500 output tokens.

func TestNewFXRate_AddsTheSpreadRoundingUp(t *testing.T) {
	cases := []struct {
		name             string
		official, spread int64
		wantEffective    int64
	}{
		{"no spread is the official rate", pegRate, 0, pegRate},
		{"2.5 percent on the peg", pegRate, 250, 3_843_750},
		{"the maximum staff may set, 10 percent", pegRate, 1000, 4_125_000},
		{"a fraction of a millionth rounds up", 3_750_001, 1, 3_750_377}, // 3,750,376.0001
		{"an exact result is not rounded", 3_750_000, 1, 3_750_375},
		{"parity", atParity, 0, atParity},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx, err := newFXRate(c.official, c.spread)
			if err != nil || fx.effectiveE6 != c.wantEffective || fx.officialE6 != c.official || fx.spreadBps != c.spread {
				t.Fatalf("newFXRate(%d, %d) = %+v, %v; want effective %d", c.official, c.spread, fx, err, c.wantEffective)
			}
		})
	}
	for _, bad := range [][2]int64{{0, 0}, {-1, 0}, {pegRate, -1}} {
		if fx, err := newFXRate(bad[0], bad[1]); err == nil {
			t.Errorf("newFXRate(%d, %d) = %+v, want an error", bad[0], bad[1], fx)
		}
	}
}

func TestHooks_HoldAndChargeAreConvertedAtTheEffectiveRate(t *testing.T) {
	ledger := &fakeLedger{balance: 100 * usd}
	parity := testPlugin(ledger, testPricer())
	peg := withRate(testPlugin(ledger, testPricer()), pegRate, 0)

	parity.PreLLMHook(testContext(testOrg), chatRequest(schemas.ChatCompletionRequest, nil))
	ctx := testContext(testOrg)
	if _, sc, _ := peg.PreLLMHook(ctx, chatRequest(schemas.ChatCompletionRequest, nil)); sc != nil {
		t.Fatalf("refused: %+v", sc.Error)
	}
	// the same worst case in USD, held in SAR at 3.75 and rounded up
	if want := (ledger.reserved[0]*375 + 99) / 100; ledger.reserved[1] < want-1 || ledger.reserved[1] > want+1 {
		t.Fatalf("held %d micro-SAR, want about %d (%d micro-USD x 3.75)", ledger.reserved[1], want, ledger.reserved[0])
	}
	if ledger.fxs[1] != fxAt(pegRate, 0) {
		t.Fatalf("the hold was recorded at %+v, want the peg", ledger.fxs[1])
	}

	peg.PostLLMHook(ctx, chatResponse(schemas.ChatCompletionRequest, 1000, 500), nil)
	st := ledger.settlements()[0]
	if st.Cost != 7500 || st.CostMicroUSD != 2000 || st.FX != fxAt(pegRate, 0) {
		t.Fatalf("charged %d micro-SAR for %d micro-USD at %+v; want 7500 for 2000 at the peg", st.Cost, st.CostMicroUSD, st.FX)
	}
	if st.Cost > ledger.reserved[1] {
		t.Fatalf("charged %d, more than the hold %d", st.Cost, ledger.reserved[1])
	}

	// with the bank's 2.5 percent: 0.002 x 3.84375 = 0.0076875 SAR
	spread := withRate(testPlugin(ledger, testPricer()), pegRate, 250)
	ctx = testContext(testOrg)
	spread.PreLLMHook(ctx, chatRequest(schemas.ChatCompletionRequest, nil))
	spread.PostLLMHook(ctx, chatResponse(schemas.ChatCompletionRequest, 1000, 500), nil)
	if st := ledger.settlements()[1]; st.Cost != 7688 || st.FX != (fxRate{officialE6: pegRate, spreadBps: 250, effectiveE6: 3_843_750}) {
		t.Fatalf("with a 2.5%% spread charged %d at %+v, want 7688 at 3.84375", st.Cost, st.FX)
	}
}

func TestHooks_ARateOrSpreadChangeDuringAnAttemptDoesNotChangeItsPrice(t *testing.T) {
	for name, change := range map[string][2]int64{
		"the official rate": {4_000_000, 0}, // 4.00 effective
		"the spread":        {pegRate, 400}, // 3.90 effective
	} {
		t.Run(name, func(t *testing.T) {
			ledger := &fakeLedger{balance: 100 * usd}
			plugin := withRate(testPlugin(ledger, testPricer()), pegRate, 0)
			ctx := testContext(testOrg)
			plugin.PreLLMHook(ctx, chatRequest(schemas.ChatCompletionRequest, nil))

			withRate(plugin, change[0], change[1]) // staff change it while the attempt runs
			plugin.PostLLMHook(ctx, chatResponse(schemas.ChatCompletionRequest, 1000, 500), nil)
			if st := ledger.settlements()[0]; st.Cost != 7500 || st.FX != fxAt(pegRate, 0) {
				t.Fatalf("charged %d at %+v, want 7500 at the peg: the rate the hold was taken at", st.Cost, st.FX)
			}

			// the next attempt converts at the new effective rate: 0.002 USD x rate
			next := testContext(testOrg)
			plugin.PreLLMHook(next, chatRequest(schemas.ChatCompletionRequest, nil))
			plugin.PostLLMHook(next, chatResponse(schemas.ChatCompletionRequest, 1000, 500), nil)
			want := fxAt(change[0], change[1])
			wantCost := want.effectiveE6 * 2000 / microPerUnit
			if st := ledger.settlements()[1]; st.Cost != wantCost || st.FX != want || ledger.fxs[1] != want {
				t.Fatalf("the next attempt was charged %d at %+v (held at %+v), want %d at %+v", st.Cost, st.FX, ledger.fxs[1], wantCost, want)
			}
		})
	}
}

func TestPostLLMHook_ChargesHalfUpToTheMicroSAR(t *testing.T) {
	for _, c := range []struct {
		costUSD float64
		want    int64
	}{
		{0.0000002, 1}, // 0.75 micro-SAR
		{0.0000001, 0}, // 0.375 micro-SAR: nothing to charge, the hold is released
		{0.000123, 461},
	} {
		ledger := &fakeLedger{balance: 100 * usd}
		pricer := testPricer()
		pricer.cost = &c.costUSD
		plugin := withRate(testPlugin(ledger, pricer), pegRate, 0)
		ctx := testContext(testOrg)
		plugin.PreLLMHook(ctx, chatRequest(schemas.ChatCompletionRequest, nil))
		plugin.PostLLMHook(ctx, chatResponse(schemas.ChatCompletionRequest, 1, 1), nil)
		if st := ledger.settlements()[0]; st.Cost != c.want {
			t.Errorf("a cost of %v USD was charged %d micro-SAR, want %d", c.costUSD, st.Cost, c.want)
		}
	}
}

func TestPreLLMHook_HoldsAtTheEffectiveRateWithTheSpread(t *testing.T) {
	ledger := &fakeLedger{balance: 100 * usd}
	withRate(testPlugin(ledger, testPricer()), pegRate, 0).PreLLMHook(testContext(testOrg), chatRequest(schemas.ChatCompletionRequest, nil))
	withRate(testPlugin(ledger, testPricer()), pegRate, 1000).PreLLMHook(testContext(testOrg), chatRequest(schemas.ChatCompletionRequest, nil))
	// the same worst case at 3.75 and at 3.75 + 10% = 4.125
	if want := (ledger.reserved[0]*11 + 9) / 10; ledger.reserved[1] < want-1 || ledger.reserved[1] > want+1 {
		t.Fatalf("held %d with a 10%% spread, want about %d (%d without)", ledger.reserved[1], want, ledger.reserved[0])
	}
	if ledger.fxs[1] != (fxRate{officialE6: pegRate, spreadBps: 1000, effectiveE6: 4_125_000}) {
		t.Fatalf("the hold was recorded at %+v", ledger.fxs[1])
	}
}
