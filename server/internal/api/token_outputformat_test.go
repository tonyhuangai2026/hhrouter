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

const tokFmtSecret = "0123456789abcdef0123456789abcdef"

// TestTokenOutputFormat_CreateValidationAndRoundTrip drives POST /api/tokens
// through the real router + JWTAuth chain: a valid output_format persists and is
// echoed back; an invalid one is rejected 400; empty/absent is accepted (follow
// endpoint).
func TestTokenOutputFormat_CreateValidationAndRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dsn := "file:tokfmt?mode=memory&cache=shared"
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&model.User{}, &model.Token{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	r := New(Deps{DB: gdb, Redis: nil, JWTSecret: tokFmtSecret, SecretKey: tokFmtSecret})
	tok, _ := middleware.IssueToken(tokFmtSecret, 1, model.RoleUser)

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/tokens", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// Valid: bedrock → 201 + echoed.
	w := post(`{"name":"k1","output_format":"bedrock"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("valid bedrock: code=%d body=%s", w.Code, w.Body.String())
	}
	var created struct {
		OutputFormat *string `json:"output_format"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if created.OutputFormat == nil || *created.OutputFormat != "bedrock" {
		t.Errorf("created output_format = %v, want bedrock", created.OutputFormat)
	}

	// Invalid: garbage → 400.
	if w := post(`{"name":"k2","output_format":"grpc"}`); w.Code != http.StatusBadRequest {
		t.Errorf("invalid output_format: code=%d want 400 body=%s", w.Code, w.Body.String())
	}

	// Absent → 201 (follow endpoint).
	if w := post(`{"name":"k3"}`); w.Code != http.StatusCreated {
		t.Errorf("absent output_format: code=%d want 201", w.Code)
	}

	// Empty string → 201 (follow endpoint).
	if w := post(`{"name":"k4","output_format":""}`); w.Code != http.StatusCreated {
		t.Errorf("empty output_format: code=%d want 201", w.Code)
	}

	// Update path also validates: create a token, then PUT a bad output_format → 400.
	var createdID struct {
		Token struct {
			ID uint `json:"id"`
		} `json:"token"`
		ID uint `json:"id"`
	}
	cw := post(`{"name":"k5","output_format":"openai"}`)
	_ = json.Unmarshal(cw.Body.Bytes(), &createdID)
	id := createdID.Token.ID
	if id == 0 {
		id = createdID.ID
	}
	if id == 0 {
		t.Fatalf("could not parse created token id from %s", cw.Body.String())
	}
	put := func(idv uint, body string) int {
		req := httptest.NewRequest(http.MethodPut, "/api/tokens/"+itoaUint(idv), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	if code := put(id, `{"output_format":"grpc"}`); code != http.StatusBadRequest {
		t.Errorf("update invalid output_format: code=%d want 400", code)
	}
	if code := put(id, `{"output_format":"anthropic"}`); code != http.StatusOK {
		t.Errorf("update valid output_format: code=%d want 200", code)
	}
}
