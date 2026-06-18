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

	"github.com/agent-router/server/internal/crypto"
	"github.com/agent-router/server/internal/middleware"
	"github.com/agent-router/server/internal/model"
)

const routeSecret = "0123456789abcdef0123456789abcdef"

// TestTestChatRoute_Auth verifies the POST /api/channels/:id/test-chat route is
// mounted behind the real JWTAuth+AdminOnly chain (and NOT behind the Quota
// middleware): no token → 401, non-admin → 403, admin → reaches the handler
// (here the upstream is unreachable so the handler returns 502, proving the
// admin request passed auth and entered the handler without a quota gate).
func TestTestChatRoute_Auth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dsn := "file:tcroute?mode=memory&cache=shared"
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Channel{}, &model.Token{}, &model.User{}, &model.RoutingRule{}, &model.RequestLog{}, &model.ModelPrice{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	enc, err := crypto.Encrypt(routeSecret, "sk-upstream")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	mj, _ := json.Marshal([]string{"gpt-4o"})
	ch := &model.Channel{Name: "oa", Type: model.ChannelOpenAI, BaseURL: "http://127.0.0.1:1", Key: enc, Models: mj, Status: model.ChannelEnabled}
	if err := gdb.Create(ch).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	// Configure a price so the admin path passes the USD price gate and proceeds
	// to the (unreachable) upstream — this test asserts auth/route wiring, not the
	// gate. Without a price the gate would short-circuit at 400.
	if err := gdb.Create(&model.ModelPrice{ChannelID: ch.ID, Model: "gpt-4o", InputMicroUSDPerM: 3_000_000, OutputMicroUSDPerM: 15_000_000}).Error; err != nil {
		t.Fatalf("create price: %v", err)
	}

	r := New(Deps{DB: gdb, Redis: nil, JWTSecret: routeSecret, SecretKey: routeSecret})

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	path := "/api/channels/" + itoaUint(ch.ID) + "/test-chat"

	call := func(token string) int {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}

	// No token → 401.
	if code := call(""); code != http.StatusUnauthorized {
		t.Fatalf("no token: code=%d want 401", code)
	}

	// Non-admin → 403.
	userTok, err := middleware.IssueToken(routeSecret, 2, model.RoleUser)
	if err != nil {
		t.Fatalf("issue user token: %v", err)
	}
	if code := call(userTok); code != http.StatusForbidden {
		t.Fatalf("non-admin: code=%d want 403", code)
	}

	// Admin → passes auth, no quota gate; upstream unreachable → 502.
	adminTok, err := middleware.IssueToken(routeSecret, 1, model.RoleAdmin)
	if err != nil {
		t.Fatalf("issue admin token: %v", err)
	}
	if code := call(adminTok); code != http.StatusBadGateway {
		t.Fatalf("admin: code=%d want 502 (handler reached, upstream unreachable)", code)
	}
}

func itoaUint(u uint) string {
	if u == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for u > 0 {
		i--
		b[i] = byte('0' + u%10)
		u /= 10
	}
	return string(b[i:])
}
