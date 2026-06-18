package service

import (
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/adapter"
	"github.com/agent-router/server/internal/model"
)

func newPricingDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:pricetest_" + t.Name() + "?mode=memory&cache=shared"
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&model.ModelPrice{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return gdb
}

// TestPricingCost covers the cost formula across the buckets, the zero-cache and
// zero-price cases, and the sub-micro rounding (ceil) behavior.
func TestPricingCost(t *testing.T) {
	svc := NewPricingService(newPricingDB(t))

	// $3/1M input, $15/1M output, $0.30/1M cache-read, $3.75/1M cache-write.
	price := &model.ModelPrice{
		InputMicroUSDPerM:      3_000_000,
		OutputMicroUSDPerM:     15_000_000,
		CacheReadMicroUSDPerM:  300_000,
		CacheWriteMicroUSDPerM: 3_750_000,
	}

	cases := []struct {
		name string
		u    adapter.Usage
		want int64 // micro-USD
	}{
		{
			name: "input+output only",
			// 1000*3_000_000/1e6 = 3000 ; 500*15_000_000/1e6 = 7500 → 10500
			u:    adapter.Usage{PromptTokens: 1000, CompletionTokens: 500},
			want: 10500,
		},
		{
			name: "with cache read",
			// + 2000*300_000/1e6 = 600 → 10500+600 = 11100
			u:    adapter.Usage{PromptTokens: 1000, CompletionTokens: 500, CacheReadTokens: 2000},
			want: 11100,
		},
		{
			name: "with cache read+write",
			// + 400*3_750_000/1e6 = 1500 → 11100+1500 = 12600
			u:    adapter.Usage{PromptTokens: 1000, CompletionTokens: 500, CacheReadTokens: 2000, CacheWriteTokens: 400},
			want: 12600,
		},
		{
			name: "zero usage → 0",
			u:    adapter.Usage{},
			want: 0,
		},
		{
			name: "sub-micro rounds up to 1",
			// 1 token * 3_000_000 /1e6 = 3 micro-USD exactly; use 1 token at a
			// fractional price to force ceil: handled below with custom price.
			u:    adapter.Usage{PromptTokens: 1},
			want: 3, // 1*3_000_000/1e6 = 3 exactly
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := svc.Cost(price, tc.u); got != tc.want {
				t.Errorf("Cost(%+v) = %d, want %d", tc.u, got, tc.want)
			}
		})
	}

	// ceilDiv: a price that yields a fractional micro-USD must round UP.
	// 1 token at 500_000 micro-USD/1M = 0.5 micro-USD → ceil → 1.
	frac := &model.ModelPrice{InputMicroUSDPerM: 500_000, OutputMicroUSDPerM: 1}
	if got := svc.Cost(frac, adapter.Usage{PromptTokens: 1}); got != 1 {
		t.Errorf("sub-micro ceilDiv = %d, want 1", got)
	}

	// zero cache PRICE with non-zero cache tokens contributes nothing.
	noCachePrice := &model.ModelPrice{InputMicroUSDPerM: 3_000_000, OutputMicroUSDPerM: 15_000_000}
	got := svc.Cost(noCachePrice, adapter.Usage{PromptTokens: 1000, CacheReadTokens: 9999, CacheWriteTokens: 9999})
	if got != 3000 {
		t.Errorf("cache tokens with zero cache price = %d, want 3000 (input only)", got)
	}

	// nil price → 0.
	if got := svc.Cost(nil, adapter.Usage{PromptTokens: 1000}); got != 0 {
		t.Errorf("nil price = %d, want 0", got)
	}
}

// TestPricingLookup covers the ErrPriceNotConfigured sentinel for a missing row
// and for rows missing input/output, plus a happy-path lookup.
func TestPricingLookup(t *testing.T) {
	gdb := newPricingDB(t)
	svc := NewPricingService(gdb)

	// (a) no row → ErrPriceNotConfigured.
	if _, err := svc.Lookup(1, "gpt-4o"); err != ErrPriceNotConfigured {
		t.Errorf("missing row: err = %v, want ErrPriceNotConfigured", err)
	}

	// (b) row with input=0 → ErrPriceNotConfigured.
	gdb.Create(&model.ModelPrice{ChannelID: 1, Model: "no-input", InputMicroUSDPerM: 0, OutputMicroUSDPerM: 15_000_000})
	if _, err := svc.Lookup(1, "no-input"); err != ErrPriceNotConfigured {
		t.Errorf("input=0: err = %v, want ErrPriceNotConfigured", err)
	}

	// (c) row with output=0 → ErrPriceNotConfigured.
	gdb.Create(&model.ModelPrice{ChannelID: 1, Model: "no-output", InputMicroUSDPerM: 3_000_000, OutputMicroUSDPerM: 0})
	if _, err := svc.Lookup(1, "no-output"); err != ErrPriceNotConfigured {
		t.Errorf("output=0: err = %v, want ErrPriceNotConfigured", err)
	}

	// (d) valid row → returned.
	gdb.Create(&model.ModelPrice{ChannelID: 1, Model: "gpt-4o", InputMicroUSDPerM: 3_000_000, OutputMicroUSDPerM: 15_000_000})
	p, err := svc.Lookup(1, "gpt-4o")
	if err != nil {
		t.Fatalf("valid lookup: %v", err)
	}
	if p.InputMicroUSDPerM != 3_000_000 || p.OutputMicroUSDPerM != 15_000_000 {
		t.Errorf("lookup row = %+v", p)
	}
}

// TestPricingUpsertAndList covers create-then-update on the (channel, model) key
// and ListByChannel.
func TestPricingUpsertAndList(t *testing.T) {
	svc := NewPricingService(newPricingDB(t))

	// create
	if _, err := svc.Upsert(7, "claude", 3_000_000, 15_000_000, 300_000, 3_750_000); err != nil {
		t.Fatalf("upsert create: %v", err)
	}
	// update same key
	if _, err := svc.Upsert(7, "claude", 4_000_000, 20_000_000, 0, 0); err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	rows, err := svc.ListByChannel(7)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (upsert must not duplicate)", len(rows))
	}
	if rows[0].InputMicroUSDPerM != 4_000_000 || rows[0].OutputMicroUSDPerM != 20_000_000 {
		t.Errorf("after update row = %+v", rows[0])
	}

	// delete
	if err := svc.Delete(rows[0].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := svc.Delete(rows[0].ID); err != ErrPriceNotConfigured {
		t.Errorf("double delete: err = %v, want ErrPriceNotConfigured", err)
	}
}
