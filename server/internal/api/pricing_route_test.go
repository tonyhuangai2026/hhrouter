package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/middleware"
	"github.com/agent-router/server/internal/model"
)

const pricingSecret = "0123456789abcdef0123456789abcdef"

// TestPricingRoutes covers the /api/pricing admin endpoints end-to-end through
// the real router + JWTAuth+AdminOnly chain: auth gating (401/403), upsert→list
// round-trip, validation (missing/negative input/output → 400), and delete.
func TestPricingRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dsn := "file:pricingroute?mode=memory&cache=shared"
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Channel{}, &model.User{}, &model.Token{}, &model.RoutingRule{}, &model.RequestLog{}, &model.ModelPrice{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	r := New(Deps{DB: gdb, Redis: nil, JWTSecret: pricingSecret, SecretKey: pricingSecret})

	adminTok, _ := middleware.IssueToken(pricingSecret, 1, model.RoleAdmin)
	userTok, _ := middleware.IssueToken(pricingSecret, 2, model.RoleUser)

	do := func(method, path, token, body string) *httptest.ResponseRecorder {
		var rdr *strings.Reader
		if body == "" {
			rdr = strings.NewReader("")
		} else {
			rdr = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, rdr)
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// --- auth gating ---
	if w := do(http.MethodGet, "/api/pricing", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: code=%d want 401", w.Code)
	}
	if w := do(http.MethodGet, "/api/pricing", userTok, ""); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin: code=%d want 403", w.Code)
	}

	// --- upsert (create) ---
	upsertBody := `{"channel_id":1,"model":"gpt-4o","input_micro_usd_per_m":3000000,"output_micro_usd_per_m":15000000,"cache_read_micro_usd_per_m":300000,"cache_write_micro_usd_per_m":3750000}`
	if w := do(http.MethodPut, "/api/pricing", adminTok, upsertBody); w.Code != http.StatusOK {
		t.Fatalf("upsert create: code=%d body=%s", w.Code, w.Body.String())
	}

	// --- upsert (update same key, no duplicate) ---
	if w := do(http.MethodPut, "/api/pricing", adminTok, `{"channel_id":1,"model":"gpt-4o","input_micro_usd_per_m":4000000,"output_micro_usd_per_m":20000000}`); w.Code != http.StatusOK {
		t.Fatalf("upsert update: code=%d body=%s", w.Code, w.Body.String())
	}

	// --- list by channel: exactly one row, with the updated price ---
	w := do(http.MethodGet, "/api/pricing?channel_id=1", adminTok, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list: code=%d body=%s", w.Code, w.Body.String())
	}
	var listResp struct {
		Data  []model.ModelPrice `json:"data"`
		Total int                `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if listResp.Total != 1 || len(listResp.Data) != 1 {
		t.Fatalf("list total=%d len=%d, want 1 (upsert must not duplicate)", listResp.Total, len(listResp.Data))
	}
	row := listResp.Data[0]
	if row.InputMicroUSDPerM != 4_000_000 || row.OutputMicroUSDPerM != 20_000_000 {
		t.Errorf("listed row = %+v, want updated 4e6/20e6", row)
	}

	// --- validation: missing input/output → 400 ---
	if w := do(http.MethodPut, "/api/pricing", adminTok, `{"channel_id":1,"model":"x","output_micro_usd_per_m":1}`); w.Code != http.StatusBadRequest {
		t.Errorf("missing input: code=%d want 400", w.Code)
	}
	if w := do(http.MethodPut, "/api/pricing", adminTok, `{"channel_id":1,"model":"x","input_micro_usd_per_m":1}`); w.Code != http.StatusBadRequest {
		t.Errorf("missing output: code=%d want 400", w.Code)
	}
	// --- validation: negative → 400 ---
	if w := do(http.MethodPut, "/api/pricing", adminTok, `{"channel_id":1,"model":"x","input_micro_usd_per_m":-1,"output_micro_usd_per_m":1}`); w.Code != http.StatusBadRequest {
		t.Errorf("negative input: code=%d want 400", w.Code)
	}

	// --- delete ---
	if w := do(http.MethodDelete, "/api/pricing/"+itoaUint(row.ID), adminTok, ""); w.Code != http.StatusOK {
		t.Fatalf("delete: code=%d body=%s", w.Code, w.Body.String())
	}
	// gone → 404 on second delete.
	if w := do(http.MethodDelete, "/api/pricing/"+itoaUint(row.ID), adminTok, ""); w.Code != http.StatusNotFound {
		t.Errorf("second delete: code=%d want 404", w.Code)
	}
}
