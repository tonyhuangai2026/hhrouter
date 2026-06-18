package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/db"
	"github.com/agent-router/server/internal/middleware"
	"github.com/agent-router/server/internal/model"
	"github.com/agent-router/server/internal/service"
)

// TestAdminUserManagement_E2E_Postgres drives the full admin user-management
// surface over real HTTP against a live Postgres + Redis (the ephemeral docker
// pair used in verification). It is skipped unless TEST_POSTGRES_DSN and
// TEST_REDIS_ADDR are set, so the default infra-free `go test` run is unaffected.
//
// It asserts, end to end: AutoMigrate adds the new columns to an already
// populated table without breaking the existing row (group backfills 'default',
// last_login NULL); paginated/filtered List with the {items,total,page,page_size}
// shape and password-free items; create/edit(+group)/delete(+token cascade);
// reset-password (returned plaintext logs in); quota add/set/reset_used with
// reset_used clearing BOTH the DB used_quota AND the Redis counter; self
// protection (delete/disable/demote self, delete last admin); last_login_at set
// on login; token group inheritance; and the admin ?user_id token dimension.
func TestAdminUserManagement_E2E_Postgres(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	addr := os.Getenv("TEST_REDIS_ADDR")
	if dsn == "" || addr == "" {
		t.Skip("TEST_POSTGRES_DSN and TEST_REDIS_ADDR required; skipping e2e")
	}
	gin.SetMode(gin.TestMode)

	// --- Simulate an UPGRADED database: create the users table WITHOUT the new
	// columns, insert a pre-existing row, THEN run the real Migrate. -----------
	gdb, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Skipf("postgres open failed (%v); skipping", err)
	}
	// Clean slate.
	gdb.Exec("DROP TABLE IF EXISTS tokens, request_logs, routing_rules, options, channels, users CASCADE")
	// Build the GORM-managed schema first (so constraint/index names match what
	// AutoMigrate expects), then DROP the two new columns and insert a row — this
	// faithfully reproduces an UPGRADED database whose users table predates the
	// group/last_login_at columns, using GORM's own naming conventions.
	if err := db.AutoMigrate(gdb); err != nil {
		t.Fatalf("initial AutoMigrate: %v", err)
	}
	if err := gdb.Exec(`ALTER TABLE users DROP COLUMN "group", DROP COLUMN last_login_at`).Error; err != nil {
		t.Fatalf("drop new columns to simulate legacy schema: %v", err)
	}
	// A pre-existing row, created before the columns existed.
	if err := gdb.Exec(`INSERT INTO users (created_at,updated_at,username,password,role,status,quota,used_quota)
		VALUES (now(),now(),'legacy','x','user','enabled',0,0)`).Error; err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	// Re-run the production migration path: it must add the columns back and
	// backfill the existing row without error.
	if err := db.AutoMigrate(gdb); err != nil {
		t.Fatalf("AutoMigrate (re-add columns): %v", err)
	}
	if err := db.SeedDefaultOptions(gdb); err != nil {
		t.Fatalf("seed options: %v", err)
	}

	// The legacy row must now have group='default' and NULL last_login_at.
	var legacy model.User
	if err := gdb.Where("username = ?", "legacy").First(&legacy).Error; err != nil {
		t.Fatalf("reload legacy: %v", err)
	}
	if legacy.Group != "default" {
		t.Fatalf("legacy row group = %q after migrate, want default", legacy.Group)
	}
	if legacy.LastLoginAt != nil {
		t.Fatalf("legacy row last_login_at = %v, want NULL", legacy.LastLoginAt)
	}
	t.Logf("AutoMigrate OK: legacy row backfilled group=%q last_login=nil", legacy.Group)

	// --- Build the real router with a shared QuotaService so reset_used clears
	// the same Redis counter the relay would consume. ------------------------
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	pctx, pcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer pcancel()
	if err := rdb.Ping(pctx).Err(); err != nil {
		t.Skipf("redis %s unreachable: %v", addr, err)
	}
	rdb.FlushDB(context.Background())

	quota := service.NewQuotaService(gdb, rdb)
	const secret = "0123456789abcdef0123456789abcdef"
	r := New(Deps{DB: gdb, Redis: rdb, JWTSecret: secret, SecretKey: secret, Quota: quota})

	// Bootstrap admin via the production seed path (the first-user-is-admin
	// register rule does not apply here because the legacy row already exists, so
	// SeedAdmin is the correct way to obtain an admin), then log in for a JWT.
	if _, err := db.SeedAdmin(gdb, "rootadmin", "adminpw123"); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	adminJWT, adminID := login(t, r, "rootadmin", "adminpw123")

	// ---- helper closures bound to r/adminJWT --------------------------------
	do := func(method, path, jwt string, body any) (*httptest.ResponseRecorder, map[string]any) {
		var rdr *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, rdr)
		if jwt != "" {
			req.Header.Set("Authorization", "Bearer "+jwt)
		}
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var parsed map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &parsed)
		return w, parsed
	}

	// ---- CREATE user --------------------------------------------------------
	w, created := do(http.MethodPost, "/api/users", adminJWT, map[string]any{
		"username": "alice", "password": "alicepw1", "display_name": "Alice",
		"email": "alice@corp.com", "role": "user", "status": "enabled",
		"quota": 1000, "group": "team-a",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create user: code=%d body=%s", w.Code, w.Body.String())
	}
	if _, hasPw := created["password"]; hasPw {
		t.Fatalf("create response leaked password field: %v", created)
	}
	aliceID := uint(created["id"].(float64))
	if created["group"] != "team-a" {
		t.Fatalf("created group = %v, want team-a", created["group"])
	}
	t.Logf("CREATE OK: alice id=%d group=team-a, no password in response", aliceID)

	// Duplicate username -> 409.
	wDup, _ := do(http.MethodPost, "/api/users", adminJWT, map[string]any{"username": "alice", "password": "x"})
	if wDup.Code != http.StatusConflict {
		t.Fatalf("duplicate create code=%d, want 409", wDup.Code)
	}

	// ---- LIST: shape + pagination + search + filter + sort ------------------
	// Add a few more users for pagination/search assertions.
	for i := 0; i < 3; i++ {
		do(http.MethodPost, "/api/users", adminJWT, map[string]any{
			"username": fmt.Sprintf("bob%d", i), "password": "bobpw123", "role": "user",
		})
	}
	wList, list := do(http.MethodGet, "/api/users?page=1&page_size=2&sort=username&order=asc", adminJWT, nil)
	if wList.Code != http.StatusOK {
		t.Fatalf("list code=%d body=%s", wList.Code, wList.Body.String())
	}
	for _, k := range []string{"items", "total", "page", "page_size"} {
		if _, ok := list[k]; !ok {
			t.Fatalf("list response missing key %q: %v", k, list)
		}
	}
	items := list["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("page_size=2 returned %d items", len(items))
	}
	if first := items[0].(map[string]any); first["username"] != "alice" {
		t.Fatalf("username asc first = %v, want alice", first["username"])
	}
	// items must never carry a password.
	if _, leaked := items[0].(map[string]any)["password"]; leaked {
		t.Fatal("list item leaked password")
	}
	// total counts admin + alice + 3 bobs + legacy = 6.
	if int(list["total"].(float64)) != 6 {
		t.Fatalf("total = %v, want 6", list["total"])
	}

	// Search (ILIKE) for alice only.
	_, searchList := do(http.MethodGet, "/api/users?search=ALICE", adminJWT, nil)
	if int(searchList["total"].(float64)) != 1 {
		t.Fatalf("search=ALICE total = %v, want 1", searchList["total"])
	}
	// Role filter: only admins.
	_, roleList := do(http.MethodGet, "/api/users?role=admin", adminJWT, nil)
	if int(roleList["total"].(float64)) != 1 {
		t.Fatalf("role=admin total = %v, want 1", roleList["total"])
	}
	t.Logf("LIST OK: shape {items,total,page,page_size}, pagination/search/role/sort all hit, no password leak")

	// ---- EDIT: change group + display_name ----------------------------------
	wEdit, edited := do(http.MethodPut, fmt.Sprintf("/api/users/%d", aliceID), adminJWT, map[string]any{
		"group": "team-b", "display_name": "Alice B",
	})
	if wEdit.Code != http.StatusOK || edited["group"] != "team-b" {
		t.Fatalf("edit group: code=%d body=%s", wEdit.Code, wEdit.Body.String())
	}
	t.Logf("EDIT OK: alice group -> team-b")

	// ---- RESET PASSWORD (empty -> generated), then login with it ------------
	wReset, reset := do(http.MethodPost, fmt.Sprintf("/api/users/%d/reset-password", aliceID), adminJWT, map[string]any{})
	if wReset.Code != http.StatusOK {
		t.Fatalf("reset-password code=%d body=%s", wReset.Code, wReset.Body.String())
	}
	tempPw, _ := reset["password"].(string)
	if len(tempPw) < 12 {
		t.Fatalf("generated temp password too short: %q", tempPw)
	}
	// The temp password logs in.
	wLogin, login := do(http.MethodPost, "/api/auth/login", "", map[string]any{"username": "alice", "password": tempPw})
	if wLogin.Code != http.StatusOK || login["token"] == nil {
		t.Fatalf("login with temp password failed: code=%d body=%s", wLogin.Code, wLogin.Body.String())
	}
	t.Logf("RESET-PASSWORD OK: generated temp pw returned once and logs in")

	// ---- QUOTA: add / set(-1) / reset_used (DB + Redis) ---------------------
	wAdd, added := do(http.MethodPost, fmt.Sprintf("/api/users/%d/quota", aliceID), adminJWT, map[string]any{"op": "add", "amount": 500})
	if wAdd.Code != http.StatusOK || int64(added["quota"].(float64)) != 1500 {
		t.Fatalf("quota add: code=%d quota=%v want 1500", wAdd.Code, added["quota"])
	}
	wSet, set := do(http.MethodPost, fmt.Sprintf("/api/users/%d/quota", aliceID), adminJWT, map[string]any{"op": "set", "amount": -1})
	if wSet.Code != http.StatusOK || int64(set["quota"].(float64)) != -1 {
		t.Fatalf("quota set -1: code=%d quota=%v", wSet.Code, set["quota"])
	}

	// Seed used quota at both levels via the relay's quota service, then reset.
	// Create a token for alice first (it should inherit team-b).
	aliceJWT := login["token"].(string)
	wTok, tok := do(http.MethodPost, "/api/tokens", aliceJWT, map[string]any{"name": "alice-key"})
	if wTok.Code != http.StatusCreated {
		t.Fatalf("create token: code=%d body=%s", wTok.Code, wTok.Body.String())
	}
	if tok["group"] != "team-b" {
		t.Fatalf("token inherited group = %v, want team-b", tok["group"])
	}
	tokenID := uint(tok["id"].(float64))
	t.Logf("TOKEN GROUP INHERITANCE OK: alice token group = team-b (from user.group)")

	// Consume to populate DB used_quota + Redis counter.
	bg := context.Background()
	if err := quota.Consume(bg, tokenID, aliceID, 250); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if v, _ := rdb.Get(bg, fmt.Sprintf("quota:user:used:%d", aliceID)).Int64(); v != 250 {
		t.Fatalf("redis user counter before reset = %d, want 250", v)
	}

	// reset_used via the API.
	wRU, _ := do(http.MethodPost, fmt.Sprintf("/api/users/%d/quota", aliceID), adminJWT, map[string]any{"op": "reset_used"})
	if wRU.Code != http.StatusOK {
		t.Fatalf("reset_used code=%d body=%s", wRU.Code, wRU.Body.String())
	}
	// DB used_quota = 0.
	var aliceReloaded model.User
	gdb.First(&aliceReloaded, aliceID)
	if aliceReloaded.UsedQuota != 0 {
		t.Fatalf("DB used_quota after reset_used = %d, want 0", aliceReloaded.UsedQuota)
	}
	// Redis key cleared (GET -> redis.Nil).
	if _, err := rdb.Get(bg, fmt.Sprintf("quota:user:used:%d", aliceID)).Result(); err != redis.Nil {
		t.Fatalf("redis user counter after reset_used not cleared: err=%v", err)
	}
	// Next quota read lazily rebuilds the counter from the (zero) DB value.
	if _, err := quota.CheckRemaining(bg, tokenID, aliceID, 1); err != nil {
		t.Fatalf("CheckRemaining after reset: %v", err)
	}
	if v, _ := rdb.Get(bg, fmt.Sprintf("quota:user:used:%d", aliceID)).Int64(); v != 0 {
		t.Fatalf("redis user counter rebuilt = %d, want 0", v)
	}
	t.Logf("QUOTA OK: add->1500, set->-1; reset_used cleared DB used_quota AND Redis quota:user:used:%d, rebuilt to 0 from DB", aliceID)

	// ---- TOKEN admin ?user_id dimension -------------------------------------
	// Admin can see alice's token via ?user_id.
	wAdminTok, adminTokList := do(http.MethodGet, fmt.Sprintf("/api/tokens?user_id=%d", aliceID), adminJWT, nil)
	if wAdminTok.Code != http.StatusOK {
		t.Fatalf("admin list alice tokens code=%d", wAdminTok.Code)
	}
	if int(adminTokList["total"].(float64)) != 1 {
		t.Fatalf("admin ?user_id token total = %v, want 1", adminTokList["total"])
	}
	// A non-admin passing ?user_id for another user is forced to their own uid:
	// alice asking for admin's tokens sees only her own (1), not admin's (0).
	wSelfTok, selfTokList := do(http.MethodGet, fmt.Sprintf("/api/tokens?user_id=%d", adminID), aliceJWT, nil)
	if wSelfTok.Code != http.StatusOK {
		t.Fatalf("non-admin token list code=%d", wSelfTok.Code)
	}
	if int(selfTokList["total"].(float64)) != 1 {
		t.Fatalf("non-admin ?user_id ignored: total = %v, want 1 (own tokens)", selfTokList["total"])
	}
	t.Logf("TOKEN ?user_id OK: admin sees target user tokens; non-admin forced to own uid")

	// ---- SELF-PROTECTION ----------------------------------------------------
	// Delete self -> 409.
	wDelSelf, _ := do(http.MethodDelete, fmt.Sprintf("/api/users/%d", adminID), adminJWT, nil)
	if wDelSelf.Code != http.StatusConflict {
		t.Fatalf("delete self code=%d, want 409", wDelSelf.Code)
	}
	// Disable self -> 409.
	wDisSelf, _ := do(http.MethodPut, fmt.Sprintf("/api/users/%d", adminID), adminJWT, map[string]any{"status": "disabled"})
	if wDisSelf.Code != http.StatusConflict {
		t.Fatalf("disable self code=%d, want 409", wDisSelf.Code)
	}
	// Demote self -> 409.
	wDemSelf, _ := do(http.MethodPut, fmt.Sprintf("/api/users/%d", adminID), adminJWT, map[string]any{"role": "user"})
	if wDemSelf.Code != http.StatusConflict {
		t.Fatalf("demote self code=%d, want 409", wDemSelf.Code)
	}
	// Last-admin HTTP refusal. Promote alice to a second admin so we have two
	// admin JWTs to work with, then exercise the guard:
	//   - with two admins, admin2 deleting the original admin is ALLOWED;
	//   - the surviving admin (alice) is then the sole admin, and deleting that
	//     last admin is REFUSED with 409.
	// (The self-guard fires first when an admin targets itself, so the last-admin
	// path over HTTP is reached by a different admin actor deleting the lone
	// remaining admin — which we set up below.)
	do(http.MethodPost, "/api/users", adminJWT, map[string]any{"username": "admin2", "password": "admin2pw", "role": "admin"})
	_, admin2Login := do(http.MethodPost, "/api/auth/login", "", map[string]any{"username": "admin2", "password": "admin2pw"})
	admin2JWT := admin2Login["token"].(string)
	// Promote alice to admin too (three admins now: rootadmin, admin2, alice).
	do(http.MethodPut, fmt.Sprintf("/api/users/%d", aliceID), admin2JWT, map[string]any{"role": "admin"})
	_, aliceAdminLogin := do(http.MethodPost, "/api/auth/login", "", map[string]any{"username": "alice", "password": tempPw})
	aliceAdminJWT := aliceAdminLogin["token"].(string)

	// alice (admin) deletes the original rootadmin and admin2, draining down to a
	// single admin (alice). Both deletions are allowed because >1 admin exists at
	// the time of each.
	wDelRoot, _ := do(http.MethodDelete, fmt.Sprintf("/api/users/%d", adminID), aliceAdminJWT, nil)
	if wDelRoot.Code != http.StatusOK {
		t.Fatalf("alice delete rootadmin code=%d body=%s", wDelRoot.Code, wDelRoot.Body.String())
	}
	admin2ID := uint(admin2Login["user"].(map[string]any)["id"].(float64))
	wDelA2, _ := do(http.MethodDelete, fmt.Sprintf("/api/users/%d", admin2ID), aliceAdminJWT, nil)
	if wDelA2.Code != http.StatusOK {
		t.Fatalf("alice delete admin2 code=%d body=%s", wDelA2.Code, wDelA2.Body.String())
	}

	// alice is now the LAST admin. Promote a fresh user to admin to act as a
	// distinct deleter, then have them delete alice — at that instant TWO admins
	// exist (alice + actor), so it succeeds and leaves the actor as sole admin.
	// To actually hit the last-admin refusal we instead promote bob0 and have
	// bob0 attempt to delete itself-distinct alice when only those two remain:
	// after bob0 deletes alice (2 admins -> 1) we then attempt to delete bob0 via
	// alice's now-stale... no. Simplest deterministic check: with exactly one
	// admin (alice), self-delete is the only way to target the lone admin and it
	// is refused — but by the self-guard, not last-admin. To prove the last-admin
	// branch over HTTP, create one more admin (bob0), delete alice (2->1 allowed),
	// then bob0 deleting the now-last admin (bob0 itself) is self-409. Thus the
	// last-admin SERVICE guard is verified by TestDelete_LastAdminRefused; here we
	// assert the system never lost its admin and that draining is monotonic.
	var adminCount int64
	gdb.Model(&model.User{}).Where("role = ?", model.RoleAdmin).Count(&adminCount)
	if adminCount != 1 {
		t.Fatalf("admin count = %d after draining, want exactly 1 (alice)", adminCount)
	}
	// Directly exercise the last-admin guard at the service layer over the same
	// live DB to confirm the 409-mapped sentinel fires when only one admin is
	// left (a non-self actor id is used so the self-guard does not mask it).
	userSvc := service.NewUserService(gdb).WithQuotaService(quota)
	if err := userSvc.Delete(99999 /*non-existent actor != alice*/, aliceID); err == nil {
		t.Fatal("deleting the last admin succeeded; want ErrLastAdmin")
	} else if err.Error() != "cannot delete the last admin account" {
		t.Fatalf("last-admin delete err = %v, want ErrLastAdmin", err)
	}
	t.Logf("SELF-PROTECTION + LAST-ADMIN OK: self delete/disable/demote 409; last admin delete refused")

	// ---- DELETE + token cascade ---------------------------------------------
	// Recreate a disposable user with a token and delete it, asserting the token
	// is gone from the DB.
	_, victim := do(http.MethodPost, "/api/users", aliceAdminJWT, map[string]any{"username": "victim", "password": "victimpw"})
	victimID := uint(victim["id"].(float64))
	_, vLogin := do(http.MethodPost, "/api/auth/login", "", map[string]any{"username": "victim", "password": "victimpw"})
	do(http.MethodPost, "/api/tokens", vLogin["token"].(string), map[string]any{"name": "vk"})
	var vTokBefore int64
	gdb.Model(&model.Token{}).Where("user_id = ?", victimID).Count(&vTokBefore)
	if vTokBefore != 1 {
		t.Fatalf("victim token precondition = %d, want 1", vTokBefore)
	}
	wDelV, _ := do(http.MethodDelete, fmt.Sprintf("/api/users/%d", victimID), aliceAdminJWT, nil)
	if wDelV.Code != http.StatusOK {
		t.Fatalf("delete victim code=%d body=%s", wDelV.Code, wDelV.Body.String())
	}
	var vTokAfter, vUserAfter int64
	gdb.Model(&model.Token{}).Where("user_id = ?", victimID).Count(&vTokAfter)
	gdb.Model(&model.User{}).Where("id = ?", victimID).Count(&vUserAfter)
	if vTokAfter != 0 || vUserAfter != 0 {
		t.Fatalf("after delete: tokens=%d user=%d, want 0/0 (cascade)", vTokAfter, vUserAfter)
	}
	t.Logf("DELETE OK: user removed and its tokens cascaded (DB verified)")

	// ---- last_login_at updated on login -------------------------------------
	// victim is gone; use alice. Her last_login_at must be set (she logged in).
	var aliceFinal model.User
	gdb.Where("username = ?", "alice").First(&aliceFinal)
	if aliceFinal.LastLoginAt == nil {
		t.Fatal("alice last_login_at is nil after logins")
	}
	t.Logf("LAST_LOGIN OK: alice last_login_at=%v after login", aliceFinal.LastLoginAt.Format(time.RFC3339))

	// ---- non-admin is forbidden on admin routes (403) -----------------------
	// Re-create a plain user and confirm 403 on POST /api/users.
	do(http.MethodPost, "/api/users", aliceAdminJWT, map[string]any{"username": "plainjoe", "password": "joepw1234", "role": "user"})
	_, joeLogin := do(http.MethodPost, "/api/auth/login", "", map[string]any{"username": "plainjoe", "password": "joepw1234"})
	wForbidden, _ := do(http.MethodPost, "/api/users", joeLogin["token"].(string), map[string]any{"username": "x", "password": "y"})
	if wForbidden.Code != http.StatusForbidden {
		t.Fatalf("non-admin POST /api/users code=%d, want 403", wForbidden.Code)
	}
	t.Logf("RBAC OK: non-admin POST /api/users -> 403")
}

// login POSTs to /api/auth/login and returns the issued JWT plus the user id.
func login(t *testing.T, r http.Handler, username, password string) (string, uint) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"username": username, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("login %s: code=%d body=%s", username, w.Code, w.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
		User  struct {
			ID   uint           `json:"id"`
			Role model.UserRole `json:"role"`
		} `json:"user"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Token == "" {
		t.Fatalf("login %s returned no token", username)
	}
	// Sanity: confirm the JWT parses so the admin chain accepts it downstream.
	if _, err := middleware.ParseToken("0123456789abcdef0123456789abcdef", resp.Token); err != nil {
		t.Fatalf("parse issued token: %v", err)
	}
	return resp.Token, resp.User.ID
}
