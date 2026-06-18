package service

import (
	"fmt"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/model"
)

func newLogTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:logtest_%d?mode=memory&cache=shared", uniq())
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&model.User{}, &model.Channel{}, &model.Token{}, &model.RequestLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return gdb
}

// seedLogs inserts a fixed dataset:
//   - user 1: 2 success (gpt-4o, ch1), 1 error (gpt-4o, ch1)
//   - user 2: 1 success (claude, ch2)
func seedLogs(t *testing.T, db *gorm.DB) {
	t.Helper()
	db.Create(&model.User{Username: "alice"})
	db.Create(&model.User{Username: "bob"})
	db.Create(&model.Channel{Name: "openai-main", Type: model.ChannelOpenAI})
	db.Create(&model.Channel{Name: "bedrock-main", Type: model.ChannelBedrock})

	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	rows := []model.RequestLog{
		{UserID: 1, ChannelID: 1, Model: "gpt-4o", Status: model.LogSuccess, PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, LatencyMs: 200, InboundFormat: model.InboundOpenAI, CostMicroUSD: i64Ptr(1000)},
		{UserID: 1, ChannelID: 1, Model: "gpt-4o", Status: model.LogSuccess, PromptTokens: 200, CompletionTokens: 80, TotalTokens: 280, LatencyMs: 400, InboundFormat: model.InboundOpenAI, CostMicroUSD: i64Ptr(2000)},
		// Error row: no cost (NULL) — must COALESCE to 0 in the SUM.
		{UserID: 1, ChannelID: 1, Model: "gpt-4o", Status: model.LogError, PromptTokens: 10, CompletionTokens: 0, TotalTokens: 10, LatencyMs: 600, InboundFormat: model.InboundOpenAI},
		{UserID: 2, ChannelID: 2, Model: "claude-3-5", Status: model.LogSuccess, PromptTokens: 300, CompletionTokens: 100, TotalTokens: 400, LatencyMs: 800, InboundFormat: model.InboundAnthropic, CostMicroUSD: i64Ptr(4000)},
	}
	for i := range rows {
		rows[i].CreatedAt = base.Add(time.Duration(i) * time.Hour)
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed log: %v", err)
		}
	}
}

func uidPtr(u uint) *uint   { return &u }
func i64Ptr(v int64) *int64 { return &v }

func TestSummary_AdminFullVsUserScoped(t *testing.T) {
	db := newLogTestDB(t)
	seedLogs(t, db)
	svc := NewLogService(db)

	// Admin (no UserID filter) sees all 4 requests.
	all, err := svc.Summary(LogFilter{})
	if err != nil {
		t.Fatalf("Summary all: %v", err)
	}
	if all.TotalRequests != 4 {
		t.Fatalf("total = %d, want 4", all.TotalRequests)
	}
	if all.SuccessRequests != 3 {
		t.Fatalf("success = %d, want 3", all.SuccessRequests)
	}
	if all.TotalTokens != 840 {
		t.Fatalf("total tokens = %d, want 840", all.TotalTokens)
	}
	if all.PromptTokens != 610 || all.CompletionTokens != 230 {
		t.Fatalf("prompt/completion = %d/%d, want 610/230", all.PromptTokens, all.CompletionTokens)
	}
	wantRate := 3.0 / 4.0
	if all.SuccessRate != wantRate {
		t.Fatalf("success_rate = %v, want %v", all.SuccessRate, wantRate)
	}
	if all.AvgLatencyMs != 500 {
		t.Fatalf("avg latency = %v, want 500", all.AvgLatencyMs)
	}
	// Cost: 1000 + 2000 + (NULL→0) + 4000 = 7000 micro-USD.
	if all.CostMicroUSD != 7000 {
		t.Fatalf("cost = %d, want 7000 micro-USD (NULL error row coalesces to 0)", all.CostMicroUSD)
	}

	// User 1 scoped sees only their 3 requests.
	u1, err := svc.Summary(LogFilter{UserID: uidPtr(1)})
	if err != nil {
		t.Fatalf("Summary u1: %v", err)
	}
	if u1.TotalRequests != 3 || u1.TotalTokens != 440 {
		t.Fatalf("u1 total/tokens = %d/%d, want 3/440", u1.TotalRequests, u1.TotalTokens)
	}
	if u1.SuccessRequests != 2 {
		t.Fatalf("u1 success = %d, want 2", u1.SuccessRequests)
	}
}

func TestSummary_EmptyRangeZeroed(t *testing.T) {
	db := newLogTestDB(t)
	seedLogs(t, db)
	svc := NewLogService(db)

	future := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	sum, err := svc.Summary(LogFilter{Start: &future})
	if err != nil {
		t.Fatalf("Summary empty: %v", err)
	}
	if sum.TotalRequests != 0 || sum.TotalTokens != 0 || sum.SuccessRate != 0 || sum.AvgLatencyMs != 0 {
		t.Fatalf("empty range not zeroed: %+v", sum)
	}
}

func TestSummary_StatusAndModelFilter(t *testing.T) {
	db := newLogTestDB(t)
	seedLogs(t, db)
	svc := NewLogService(db)

	errOnly, _ := svc.Summary(LogFilter{Status: model.LogError})
	if errOnly.TotalRequests != 1 || errOnly.SuccessRequests != 0 {
		t.Fatalf("error filter: %+v", errOnly)
	}
	gpt, _ := svc.Summary(LogFilter{Model: "gpt-4o"})
	if gpt.TotalRequests != 3 {
		t.Fatalf("model filter total = %d, want 3", gpt.TotalRequests)
	}
}

func TestTimeseries_GroupedByModel(t *testing.T) {
	db := newLogTestDB(t)
	seedLogs(t, db)
	svc := NewLogService(db)

	groups, err := svc.Timeseries(LogFilter{}, "hour", "model")
	if err != nil {
		t.Fatalf("Timeseries: %v", err)
	}
	// Two models => two groups.
	byModel := map[string]int64{}
	for _, g := range groups {
		var reqs int64
		for _, p := range g.Points {
			reqs += p.Requests
			if p.TS == "" {
				t.Fatalf("empty bucket ts in group %q", g.Group)
			}
		}
		byModel[g.Group] = reqs
	}
	if byModel["gpt-4o"] != 3 {
		t.Fatalf("gpt-4o requests = %d, want 3", byModel["gpt-4o"])
	}
	if byModel["claude-3-5"] != 1 {
		t.Fatalf("claude requests = %d, want 1", byModel["claude-3-5"])
	}
}

func TestTimeseries_Ungrouped(t *testing.T) {
	db := newLogTestDB(t)
	seedLogs(t, db)
	svc := NewLogService(db)

	groups, err := svc.Timeseries(LogFilter{}, "day", "")
	if err != nil {
		t.Fatalf("Timeseries: %v", err)
	}
	if len(groups) != 1 || groups[0].Group != "all" {
		t.Fatalf("ungrouped should yield single 'all' group, got %d", len(groups))
	}
	// All 4 rows fall on the same day bucket.
	if len(groups[0].Points) != 1 || groups[0].Points[0].Requests != 4 {
		t.Fatalf("day bucket = %+v, want 1 point with 4 requests", groups[0].Points)
	}
	if groups[0].Points[0].Tokens != 840 {
		t.Fatalf("day bucket tokens = %d, want 840", groups[0].Points[0].Tokens)
	}
}

func TestList_PaginationFilterAndScoping(t *testing.T) {
	db := newLogTestDB(t)
	seedLogs(t, db)
	svc := NewLogService(db)

	// User 1 scoped: 3 rows, newest first; joined username present.
	rows, total, err := svc.List(LogFilter{UserID: uidPtr(1)}, 1, 2)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if len(rows) != 2 {
		t.Fatalf("page size = %d, want 2", len(rows))
	}
	if rows[0].Username != "alice" || rows[0].ChannelName != "openai-main" {
		t.Fatalf("join missing: user=%q channel=%q", rows[0].Username, rows[0].ChannelName)
	}
	// Newest first: the error row was inserted last among user 1's rows.
	if !rows[0].CreatedAt.After(rows[1].CreatedAt) {
		t.Fatalf("rows not ordered newest-first")
	}

	// Page 2 has the remaining row.
	p2, _, _ := svc.List(LogFilter{UserID: uidPtr(1)}, 2, 2)
	if len(p2) != 1 {
		t.Fatalf("page 2 len = %d, want 1", len(p2))
	}

	// Admin (no scope) sees all 4.
	_, allTotal, _ := svc.List(LogFilter{}, 1, 50)
	if allTotal != 4 {
		t.Fatalf("admin total = %d, want 4", allTotal)
	}
}

// TestList_TokenNameJoin verifies the listing surfaces the API key (token) name
// for keyed production rows, and leaves it empty for token-less (test-chat) rows.
func TestList_TokenNameJoin(t *testing.T) {
	db := newLogTestDB(t)
	db.Create(&model.User{Username: "alice"})
	db.Create(&model.Channel{Name: "openai-main", Type: model.ChannelOpenAI})
	db.Create(&model.Token{UserID: 1, Name: "prod-key", KeyHash: "h1", Status: model.TokenEnabled})

	tokenID := uint(1)
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	// Keyed production row + a token-less test-chat row.
	keyed := model.RequestLog{UserID: 1, TokenID: &tokenID, ChannelID: 1, Model: "gpt-4o", Status: model.LogSuccess, InboundFormat: model.InboundOpenAI, CreatedAt: base.Add(time.Hour)}
	test := model.RequestLog{UserID: 1, TokenID: nil, ChannelID: 1, Model: "gpt-4o", Status: model.LogSuccess, InboundFormat: model.InboundOpenAI, IsTest: true, CreatedAt: base}
	if err := db.Create(&keyed).Error; err != nil {
		t.Fatalf("seed keyed: %v", err)
	}
	if err := db.Create(&test).Error; err != nil {
		t.Fatalf("seed test: %v", err)
	}

	rows, _, err := NewLogService(db).List(LogFilter{}, 1, 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// Newest first → keyed row at index 0.
	if rows[0].TokenName != "prod-key" {
		t.Errorf("keyed row token_name = %q, want prod-key", rows[0].TokenName)
	}
	if rows[1].TokenName != "" {
		t.Errorf("test-chat row token_name = %q, want empty", rows[1].TokenName)
	}
}

func TestWrite(t *testing.T) {
	db := newLogTestDB(t)
	svc := NewLogService(db)
	log := &model.RequestLog{UserID: 1, ChannelID: 1, Model: "gpt-4o", Status: model.LogSuccess, TotalTokens: 42, InboundFormat: model.InboundOpenAI}
	if err := svc.Write(log); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if log.ID == 0 {
		t.Fatal("Write did not populate ID")
	}
	var count int64
	db.Model(&model.RequestLog{}).Count(&count)
	if count != 1 {
		t.Fatalf("row count = %d, want 1", count)
	}
}

func boolPtr(b bool) *bool { return &b }

// TestIsTestFilter locks the is_test scoping semantics used by the dashboard:
// nil = all rows, *false = production only, *true = test only. It also confirms
// a test row writes token_id NULL (a *uint nil) while a production row keeps its
// non-nil token id.
func TestIsTestFilter(t *testing.T) {
	db := newLogTestDB(t)
	svc := NewLogService(db)

	tokenID := uint(99)
	rows := []model.RequestLog{
		// 2 production rows (is_test default false), keyed by a token.
		{UserID: 1, TokenID: &tokenID, ChannelID: 1, Model: "gpt-4o", Status: model.LogSuccess, TotalTokens: 10, InboundFormat: model.InboundOpenAI},
		{UserID: 1, TokenID: &tokenID, ChannelID: 1, Model: "gpt-4o", Status: model.LogSuccess, TotalTokens: 20, InboundFormat: model.InboundOpenAI},
		// 1 test row: is_test=true, token_id nil.
		{UserID: 1, TokenID: nil, ChannelID: 1, Model: "gpt-4o", Status: model.LogSuccess, TotalTokens: 5, InboundFormat: model.InboundOpenAI, IsTest: true},
	}
	for i := range rows {
		if err := svc.Write(&rows[i]); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	// nil → all 3.
	if _, total, _ := svc.List(LogFilter{}, 1, 50); total != 3 {
		t.Fatalf("nil filter total = %d, want 3 (all)", total)
	}
	// *false → production only (2).
	if _, total, _ := svc.List(LogFilter{IsTest: boolPtr(false)}, 1, 50); total != 2 {
		t.Fatalf("is_test=false total = %d, want 2 (prod only)", total)
	}
	// *true → test only (1).
	if _, total, _ := svc.List(LogFilter{IsTest: boolPtr(true)}, 1, 50); total != 1 {
		t.Fatalf("is_test=true total = %d, want 1 (test only)", total)
	}

	// Summary respects the same scoping: production-only excludes the test row.
	prodSum, err := svc.Summary(LogFilter{IsTest: boolPtr(false)})
	if err != nil {
		t.Fatalf("Summary prod: %v", err)
	}
	if prodSum.TotalRequests != 2 || prodSum.TotalTokens != 30 {
		t.Fatalf("prod summary = %d req / %d tokens, want 2/30", prodSum.TotalRequests, prodSum.TotalTokens)
	}
	allSum, _ := svc.Summary(LogFilter{})
	if allSum.TotalRequests != 3 || allSum.TotalTokens != 35 {
		t.Fatalf("all summary = %d req / %d tokens, want 3/35", allSum.TotalRequests, allSum.TotalTokens)
	}

	// The test row's token_id is SQL NULL; the production rows are not.
	var nullCount int64
	db.Model(&model.RequestLog{}).Where("token_id IS NULL").Count(&nullCount)
	if nullCount != 1 {
		t.Fatalf("token_id IS NULL count = %d, want 1", nullCount)
	}
}
