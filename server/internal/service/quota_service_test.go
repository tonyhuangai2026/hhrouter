package service

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/model"
)

// uniqSeq yields process-unique suffixes so seeded usernames never collide
// across tests sharing an in-memory DB.
var uniqSeq int64

func uniq() int64 { return atomic.AddInt64(&uniqSeq, 1) }

// --- pure logic tests (no infra) ---------------------------------------------

func TestRemaining(t *testing.T) {
	cases := []struct {
		name        string
		quota, used int64
		want        int64
	}{
		{"unlimited", model.QuotaUnlimited, 100, -1},
		{"headroom", 100, 30, 70},
		{"exact", 100, 100, 0},
		{"overdrawn clamps to 0", 100, 130, 0},
		{"zero quota", 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := remaining(c.quota, c.used); got != c.want {
				t.Fatalf("remaining(%d,%d) = %d, want %d", c.quota, c.used, got, c.want)
			}
		})
	}
}

func TestMinRemaining(t *testing.T) {
	cases := []struct {
		name string
		a, b int64
		want int64
	}{
		{"both unlimited", -1, -1, -1},
		{"a unlimited -> b", -1, 50, 50},
		{"b unlimited -> a", 50, -1, 50},
		{"a smaller", 10, 50, 10},
		{"b smaller", 80, 20, 20},
		{"equal", 30, 30, 30},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := minRemaining(c.a, c.b); got != c.want {
				t.Fatalf("minRemaining(%d,%d) = %d, want %d", c.a, c.b, got, c.want)
			}
		})
	}
}

// --- DB-backed tests (sqlite in-memory, rdb=nil) -----------------------------

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	// A private, named in-memory DB per test so data never bleeds between tests.
	dsn := fmt.Sprintf("file:quota_%d?mode=memory&cache=shared", uniq())
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&model.User{}, &model.Token{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return gdb
}

func seedUserToken(t *testing.T, gdb *gorm.DB, userQuota, userUsed, tokenQuota, tokenUsed int64) (uint, uint) {
	t.Helper()
	uname := fmt.Sprintf("u%d", uniq())
	u := &model.User{Username: uname, Password: "x", Quota: userQuota, UsedQuota: userUsed, Status: model.UserEnabled}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	tok := &model.Token{UserID: u.ID, Name: "k", KeyHash: HashKey(fmt.Sprintf("sk-test-%d", uniq())),
		Status: model.TokenEnabled, Quota: tokenQuota, UsedQuota: tokenUsed, Group: "default"}
	if err := gdb.Create(tok).Error; err != nil {
		t.Fatalf("create token: %v", err)
	}
	return tok.ID, u.ID
}

func TestCheckRemaining_DBOnly_MinAcrossLevels(t *testing.T) {
	gdb := newTestDB(t)
	qs := NewQuotaService(gdb, nil)
	ctx := context.Background()

	// token remaining = 100-40 = 60; user remaining = 1000-980 = 20 -> min = 20.
	tokenID, userID := seedUserToken(t, gdb, 1000, 980, 100, 40)
	rem, err := qs.CheckRemaining(ctx, tokenID, userID, 10)
	if err != nil {
		t.Fatalf("CheckRemaining: %v", err)
	}
	if rem != 20 {
		t.Fatalf("remaining = %d, want 20 (min of token 60, user 20)", rem)
	}
}

func TestCheckRemaining_DBOnly_Unlimited(t *testing.T) {
	gdb := newTestDB(t)
	qs := NewQuotaService(gdb, nil)
	ctx := context.Background()

	// token unlimited (-1), user bounded -> user governs.
	tokenID, userID := seedUserToken(t, gdb, 500, 100, model.QuotaUnlimited, 9999)
	rem, err := qs.CheckRemaining(ctx, tokenID, userID, 1)
	if err != nil {
		t.Fatalf("CheckRemaining: %v", err)
	}
	if rem != 400 {
		t.Fatalf("remaining = %d, want 400 (user 500-100)", rem)
	}

	// both unlimited -> -1 sentinel; HasRemaining always true.
	tokenID2, userID2 := seedUserToken(t, gdb, model.QuotaUnlimited, 1, model.QuotaUnlimited, 1)
	rem, err = qs.CheckRemaining(ctx, tokenID2, userID2, 1_000_000)
	if err != nil {
		t.Fatalf("CheckRemaining: %v", err)
	}
	if rem != -1 {
		t.Fatalf("remaining = %d, want -1 (both unlimited)", rem)
	}
	ok, err := qs.HasRemaining(ctx, tokenID2, userID2, 1_000_000)
	if err != nil || !ok {
		t.Fatalf("HasRemaining(unlimited) = %v, %v; want true, nil", ok, err)
	}
}

func TestConsume_DBOnly_IncrementsBothLevels(t *testing.T) {
	gdb := newTestDB(t)
	qs := NewQuotaService(gdb, nil)
	ctx := context.Background()

	tokenID, userID := seedUserToken(t, gdb, 1000, 100, 500, 50)
	if err := qs.Consume(ctx, tokenID, userID, 25); err != nil {
		t.Fatalf("Consume: %v", err)
	}

	var tok model.Token
	var u model.User
	gdb.First(&tok, tokenID)
	gdb.First(&u, userID)
	if tok.UsedQuota != 75 {
		t.Fatalf("token used_quota = %d, want 75", tok.UsedQuota)
	}
	if u.UsedQuota != 125 {
		t.Fatalf("user used_quota = %d, want 125", u.UsedQuota)
	}

	// remaining now: token 500-75=425, user 1000-125=875 -> min 425.
	rem, _ := qs.CheckRemaining(ctx, tokenID, userID, 1)
	if rem != 425 {
		t.Fatalf("remaining after consume = %d, want 425", rem)
	}

	// non-positive consume is a no-op.
	if err := qs.Consume(ctx, tokenID, userID, 0); err != nil {
		t.Fatalf("Consume(0): %v", err)
	}
	gdb.First(&tok, tokenID)
	if tok.UsedQuota != 75 {
		t.Fatalf("token used_quota after no-op = %d, want 75", tok.UsedQuota)
	}
}

func TestHasRemaining_DBOnly_Insufficient(t *testing.T) {
	gdb := newTestDB(t)
	qs := NewQuotaService(gdb, nil)
	ctx := context.Background()

	// token remaining = 10; ask for 50 -> false.
	tokenID, userID := seedUserToken(t, gdb, 1000, 0, 100, 90)
	ok, err := qs.HasRemaining(ctx, tokenID, userID, 50)
	if err != nil {
		t.Fatalf("HasRemaining: %v", err)
	}
	if ok {
		t.Fatalf("HasRemaining = true, want false (only 10 left, asked 50)")
	}
}

// --- Redis-backed tests (lazy-load, consume, restart recovery) ---------------
//
// These run only when a Redis is reachable. Set TEST_REDIS_ADDR (e.g. from the
// ephemeral docker redis used in live verification); otherwise they are skipped
// so the default `go test` run stays infra-free.

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set; skipping Redis-backed quota test")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis %s unreachable: %v", addr, err)
	}
	rdb.FlushDB(context.Background())
	return rdb
}

func TestRedis_LazyLoadFromDBAndConsume(t *testing.T) {
	rdb := testRedis(t)
	gdb := newTestDB(t)
	qs := NewQuotaService(gdb, rdb)
	ctx := context.Background()

	// DB already records prior usage. Redis is empty (cold), so the counter must
	// be lazily seeded from DB used_quota before the new consume is applied.
	tokenID, userID := seedUserToken(t, gdb, 1000, 200, 500, 300)

	// First read lazily backfills counters from DB.
	rem, err := qs.CheckRemaining(ctx, tokenID, userID, 1)
	if err != nil {
		t.Fatalf("CheckRemaining: %v", err)
	}
	// token remaining 500-300=200; user 1000-200=800 -> min 200.
	if rem != 200 {
		t.Fatalf("remaining = %d, want 200 (lazy-loaded from DB)", rem)
	}

	// Redis counters now reflect DB used_quota.
	if v, _ := rdb.Get(ctx, tokenUsedKey(tokenID)).Int64(); v != 300 {
		t.Fatalf("token counter = %d, want 300 (seeded from DB)", v)
	}
	if v, _ := rdb.Get(ctx, userUsedKey(userID)).Int64(); v != 200 {
		t.Fatalf("user counter = %d, want 200 (seeded from DB)", v)
	}

	// Consume builds on the lazily-loaded base.
	if err := qs.Consume(ctx, tokenID, userID, 50); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if v, _ := rdb.Get(ctx, tokenUsedKey(tokenID)).Int64(); v != 350 {
		t.Fatalf("token counter after consume = %d, want 350", v)
	}
	if v, _ := rdb.Get(ctx, userUsedKey(userID)).Int64(); v != 250 {
		t.Fatalf("user counter after consume = %d, want 250", v)
	}
}

func TestRedis_WriteBackAndRestartRecovery(t *testing.T) {
	rdb := testRedis(t)
	gdb := newTestDB(t)
	ctx := context.Background()

	tokenID, userID := seedUserToken(t, gdb, 1000, 0, 1000, 0)

	qs := NewQuotaService(gdb, rdb)
	if err := qs.Consume(ctx, tokenID, userID, 120); err != nil {
		t.Fatalf("Consume: %v", err)
	}

	// Flush Redis counters to DB used_quota.
	if err := qs.WriteBack(ctx); err != nil {
		t.Fatalf("WriteBack: %v", err)
	}
	var tok model.Token
	var u model.User
	gdb.First(&tok, tokenID)
	gdb.First(&u, userID)
	if tok.UsedQuota != 120 || u.UsedQuota != 120 {
		t.Fatalf("after write-back token=%d user=%d, want 120/120", tok.UsedQuota, u.UsedQuota)
	}

	// Simulate restart: Redis wiped, new service instance. Counters must rebuild
	// from DB used_quota so usage is not lost.
	rdb.FlushDB(ctx)
	qs2 := NewQuotaService(gdb, rdb)
	rem, err := qs2.CheckRemaining(ctx, tokenID, userID, 1)
	if err != nil {
		t.Fatalf("CheckRemaining post-restart: %v", err)
	}
	// remaining = min(1000-120, 1000-120) = 880.
	if rem != 880 {
		t.Fatalf("remaining post-restart = %d, want 880 (rebuilt from DB)", rem)
	}
	// And the rebuilt Redis counter equals the persisted used_quota.
	if v, _ := rdb.Get(ctx, tokenUsedKey(tokenID)).Int64(); v != 120 {
		t.Fatalf("token counter post-restart = %d, want 120", v)
	}
}
