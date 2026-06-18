package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/agent-router/server/internal/model"
)

const testSecret = "test-secret-key"

func init() {
	gin.SetMode(gin.TestMode)
}

func TestIssueAndParseToken(t *testing.T) {
	tok, err := IssueToken(testSecret, 42, model.RoleAdmin)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	claims, err := ParseToken(testSecret, tok)
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	if claims.UID != 42 {
		t.Errorf("uid = %d, want 42", claims.UID)
	}
	if claims.Role != model.RoleAdmin {
		t.Errorf("role = %q, want admin", claims.Role)
	}
}

func TestParseTokenWrongSecret(t *testing.T) {
	tok, err := IssueToken(testSecret, 1, model.RoleUser)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if _, err := ParseToken("other-secret", tok); err == nil {
		t.Fatal("expected error parsing with wrong secret, got nil")
	}
}

// newRouter builds a tiny router that mirrors the protected route wiring.
func newRouter() *gin.Engine {
	r := gin.New()
	auth := r.Group("/auth")
	auth.Use(JWTAuth(testSecret))
	auth.GET("/me", func(c *gin.Context) {
		id, _ := CurrentUserID(c)
		c.JSON(http.StatusOK, gin.H{"uid": id})
	})

	admin := r.Group("/admin")
	admin.Use(JWTAuth(testSecret), AdminOnly())
	admin.GET("/secret", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

func do(r *gin.Engine, method, path, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestJWTAuthRejectsMissingToken(t *testing.T) {
	r := newRouter()
	if w := do(r, http.MethodGet, "/auth/me", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", w.Code)
	}
}

func TestJWTAuthRejectsInvalidToken(t *testing.T) {
	r := newRouter()
	if w := do(r, http.MethodGet, "/auth/me", "garbage.token.value"); w.Code != http.StatusUnauthorized {
		t.Errorf("bad token: status = %d, want 401", w.Code)
	}
}

func TestJWTAuthAcceptsValidToken(t *testing.T) {
	r := newRouter()
	tok, _ := IssueToken(testSecret, 7, model.RoleUser)
	if w := do(r, http.MethodGet, "/auth/me", tok); w.Code != http.StatusOK {
		t.Errorf("valid token: status = %d, want 200", w.Code)
	}
}

func TestAdminOnlyForbidsRegularUser(t *testing.T) {
	r := newRouter()
	tok, _ := IssueToken(testSecret, 7, model.RoleUser)
	if w := do(r, http.MethodGet, "/admin/secret", tok); w.Code != http.StatusForbidden {
		t.Errorf("user on admin route: status = %d, want 403", w.Code)
	}
}

func TestAdminOnlyAllowsAdmin(t *testing.T) {
	r := newRouter()
	tok, _ := IssueToken(testSecret, 1, model.RoleAdmin)
	if w := do(r, http.MethodGet, "/admin/secret", tok); w.Code != http.StatusOK {
		t.Errorf("admin on admin route: status = %d, want 200", w.Code)
	}
}
