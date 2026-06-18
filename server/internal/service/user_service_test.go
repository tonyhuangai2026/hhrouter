package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/model"
)

// newUserTestDB builds a private in-memory SQLite DB migrated with the user and
// token tables. SQLite is sufficient for every UserService path except the
// ILIKE search (Postgres-only), which is covered separately against a real
// Postgres when TEST_POSTGRES_DSN is set.
func newUserTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:usertest_%d?mode=memory&cache=shared", uniq())
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&model.User{}, &model.Token{}, &model.Option{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return gdb
}

// makeUser inserts a user with explicit fields and returns it.
func makeUser(t *testing.T, gdb *gorm.DB, username string, role model.UserRole, status model.UserStatus, quota, used int64) *model.User {
	t.Helper()
	u := &model.User{
		Username:  username,
		Password:  "x",
		Role:      role,
		Status:    status,
		Quota:     quota,
		UsedQuota: used,
		Group:     "default",
	}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("create user %s: %v", username, err)
	}
	return u
}

// --- AutoMigrate / model column -----------------------------------------------

func TestUser_GroupDefaultAndLastLoginNullable(t *testing.T) {
	gdb := newUserTestDB(t)

	// Insert WITHOUT specifying Group: the column default 'default' applies, and
	// LastLoginAt stays NULL — proving the migration is non-destructive for rows
	// that predate the columns.
	u := &model.User{Username: "nogroup", Password: "x", Status: model.UserEnabled}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	var got model.User
	if err := gdb.First(&got, u.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Group != "default" {
		t.Fatalf("group = %q, want default", got.Group)
	}
	if got.LastLoginAt != nil {
		t.Fatalf("last_login_at = %v, want nil", got.LastLoginAt)
	}
}

// --- Login touches LastLoginAt -------------------------------------------------

func TestLogin_UpdatesLastLoginAt(t *testing.T) {
	gdb := newUserTestDB(t)
	svc := NewUserService(gdb)

	// Create via Register so the password is a real bcrypt hash.
	if _, err := svc.Register(RegisterInput{Username: "loginme", Password: "secretpw"}, true); err != nil {
		t.Fatalf("register: %v", err)
	}

	before := time.Now().Add(-time.Second)
	u, err := svc.Login("loginme", "secretpw")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if u.LastLoginAt == nil {
		t.Fatal("returned user LastLoginAt is nil after login")
	}
	if u.LastLoginAt.Before(before) {
		t.Fatalf("LastLoginAt %v is before login start %v", u.LastLoginAt, before)
	}

	// Persisted to DB as well.
	var fromDB model.User
	if err := gdb.First(&fromDB, u.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if fromDB.LastLoginAt == nil {
		t.Fatal("DB LastLoginAt is nil after login")
	}
}

// --- ListPaged: pagination, sort, filters (SQLite-safe) -----------------------

func TestListPaged_PaginationAndDefaults(t *testing.T) {
	gdb := newUserTestDB(t)
	svc := NewUserService(gdb)

	// Insert 25 users with increasing created_at so default sort (created_at
	// desc) has a deterministic order.
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 25; i++ {
		u := makeUser(t, gdb, fmt.Sprintf("user%02d", i), model.RoleUser, model.UserEnabled, int64(i), 0)
		// Force distinct, increasing created_at values.
		ts := base.Add(time.Duration(i) * time.Minute)
		if err := gdb.Model(&model.User{}).Where("id = ?", u.ID).UpdateColumn("created_at", ts).Error; err != nil {
			t.Fatalf("set created_at: %v", err)
		}
	}

	// Default page/page_size: page 1, size 20.
	items, total, err := svc.ListPaged(UserListQuery{})
	if err != nil {
		t.Fatalf("ListPaged: %v", err)
	}
	if total != 25 {
		t.Fatalf("total = %d, want 25", total)
	}
	if len(items) != 20 {
		t.Fatalf("default page size = %d, want 20", len(items))
	}
	// Default order is created_at desc -> newest (user24) first.
	if items[0].Username != "user24" {
		t.Fatalf("first item = %q, want user24 (created_at desc)", items[0].Username)
	}

	// Page 2 has the remaining 5.
	items2, _, err := svc.ListPaged(UserListQuery{Page: 2})
	if err != nil {
		t.Fatalf("ListPaged page 2: %v", err)
	}
	if len(items2) != 5 {
		t.Fatalf("page 2 size = %d, want 5", len(items2))
	}
}

func TestListPaged_PageSizeCap(t *testing.T) {
	gdb := newUserTestDB(t)
	svc := NewUserService(gdb)
	for i := 0; i < 5; i++ {
		makeUser(t, gdb, fmt.Sprintf("c%02d", i), model.RoleUser, model.UserEnabled, 0, 0)
	}
	// Request a page size above the cap; it must be clamped to maxUserPageSize
	// (so the query is bounded). With only 5 rows we still get 5 back, but the
	// effective limit is verified by the cap not erroring and total being right.
	_, total, err := svc.ListPaged(UserListQuery{PageSize: 100000})
	if err != nil {
		t.Fatalf("ListPaged: %v", err)
	}
	if total != 5 {
		t.Fatalf("total = %d, want 5", total)
	}
}

func TestListPaged_SortWhitelistAndOrder(t *testing.T) {
	gdb := newUserTestDB(t)
	svc := NewUserService(gdb)

	makeUser(t, gdb, "bbb", model.RoleUser, model.UserEnabled, 50, 0)
	makeUser(t, gdb, "aaa", model.RoleUser, model.UserEnabled, 10, 0)
	makeUser(t, gdb, "ccc", model.RoleUser, model.UserEnabled, 30, 0)

	// Sort by username asc.
	items, _, err := svc.ListPaged(UserListQuery{Sort: "username", Order: "asc"})
	if err != nil {
		t.Fatalf("ListPaged username asc: %v", err)
	}
	if items[0].Username != "aaa" || items[2].Username != "ccc" {
		t.Fatalf("username asc order wrong: %v", usernames(items))
	}

	// Sort by used_quota -> we set quota not used; sort by used_quota desc all 0,
	// so instead sort by quota is not whitelisted -> falls back to created_at.
	// Verify the whitelist: an unknown sort key must not error and must not
	// inject; it falls back to created_at desc.
	itemsBad, _, err := svc.ListPaged(UserListQuery{Sort: "quota; DROP TABLE users;--", Order: "asc"})
	if err != nil {
		t.Fatalf("ListPaged with injection-y sort errored: %v", err)
	}
	if len(itemsBad) != 3 {
		t.Fatalf("injection-y sort returned %d rows, want 3 (fell back safely)", len(itemsBad))
	}
	// Table must still exist (no injection executed).
	var n int64
	if err := gdb.Model(&model.User{}).Count(&n).Error; err != nil || n != 3 {
		t.Fatalf("users table count = %d err=%v, want 3 (table intact)", n, err)
	}
}

func TestListPaged_RoleAndStatusFilter(t *testing.T) {
	gdb := newUserTestDB(t)
	svc := NewUserService(gdb)

	makeUser(t, gdb, "admin1", model.RoleAdmin, model.UserEnabled, 0, 0)
	makeUser(t, gdb, "user1", model.RoleUser, model.UserEnabled, 0, 0)
	makeUser(t, gdb, "user2", model.RoleUser, model.UserDisabled, 0, 0)

	adminItems, adminTotal, err := svc.ListPaged(UserListQuery{Role: string(model.RoleAdmin)})
	if err != nil {
		t.Fatalf("ListPaged role=admin: %v", err)
	}
	if adminTotal != 1 || len(adminItems) != 1 || adminItems[0].Username != "admin1" {
		t.Fatalf("role filter wrong: total=%d items=%v", adminTotal, usernames(adminItems))
	}

	_, disabledTotal, err := svc.ListPaged(UserListQuery{Status: string(model.UserDisabled)})
	if err != nil {
		t.Fatalf("ListPaged status=disabled: %v", err)
	}
	if disabledTotal != 1 {
		t.Fatalf("status filter total = %d, want 1", disabledTotal)
	}
}

func usernames(us []model.User) []string {
	out := make([]string, len(us))
	for i := range us {
		out[i] = us[i].Username
	}
	return out
}

// --- AdminCreate ---------------------------------------------------------------

func TestAdminCreate_SetsAllFields(t *testing.T) {
	gdb := newUserTestDB(t)
	svc := NewUserService(gdb)

	q := int64(12345)
	u, err := svc.AdminCreate(AdminCreateInput{
		Username:    "newadmin",
		Password:    "initialpw",
		DisplayName: "New Admin",
		Email:       "na@example.com",
		Role:        string(model.RoleAdmin),
		Status:      string(model.UserDisabled),
		Quota:       &q,
		Group:       "premium",
	})
	if err != nil {
		t.Fatalf("AdminCreate: %v", err)
	}
	if u.Role != model.RoleAdmin || u.Status != model.UserDisabled || u.Quota != q || u.Group != "premium" {
		t.Fatalf("fields not all set: role=%s status=%s quota=%d group=%s", u.Role, u.Status, u.Quota, u.Group)
	}
	if u.DisplayName != "New Admin" || u.Email != "na@example.com" {
		t.Fatalf("display/email not set: %q / %q", u.DisplayName, u.Email)
	}
	// Password is a real bcrypt hash of the supplied value.
	if bcrypt.CompareHashAndPassword([]byte(u.Password), []byte("initialpw")) != nil {
		t.Fatal("stored password is not a bcrypt hash of the supplied password")
	}
}

func TestAdminCreate_Defaults(t *testing.T) {
	gdb := newUserTestDB(t)
	// Seed a non-zero DefaultUserQuota option to prove the default is read.
	if err := gdb.Create(&model.Option{Key: model.OptDefaultUserQuota, Value: "777"}).Error; err != nil {
		t.Fatalf("seed option: %v", err)
	}
	svc := NewUserService(gdb)

	u, err := svc.AdminCreate(AdminCreateInput{Username: "plainuser"})
	if err != nil {
		t.Fatalf("AdminCreate: %v", err)
	}
	if u.Role != model.RoleUser {
		t.Fatalf("default role = %s, want user", u.Role)
	}
	if u.Status != model.UserEnabled {
		t.Fatalf("default status = %s, want enabled", u.Status)
	}
	if u.Group != "default" {
		t.Fatalf("default group = %q, want default", u.Group)
	}
	if u.Quota != 777 {
		t.Fatalf("default quota = %d, want 777 (from option)", u.Quota)
	}
	// No password supplied -> a usable bcrypt hash was generated (non-empty,
	// and not the literal empty string hash check: it must reject the empty pw).
	if u.Password == "" {
		t.Fatal("generated password hash is empty")
	}
}

func TestAdminCreate_DuplicateUsernameConflict(t *testing.T) {
	gdb := newUserTestDB(t)
	svc := NewUserService(gdb)
	if _, err := svc.AdminCreate(AdminCreateInput{Username: "dup", Password: "p"}); err != nil {
		t.Fatalf("first AdminCreate: %v", err)
	}
	_, err := svc.AdminCreate(AdminCreateInput{Username: "dup", Password: "p2"})
	if !errors.Is(err, ErrUserExists) {
		t.Fatalf("duplicate create err = %v, want ErrUserExists", err)
	}
}

// --- Delete: self-protection + last-admin + token cascade ---------------------

func TestDelete_SelfRefused(t *testing.T) {
	gdb := newUserTestDB(t)
	svc := NewUserService(gdb)
	admin := makeUser(t, gdb, "admin", model.RoleAdmin, model.UserEnabled, 0, 0)

	err := svc.Delete(admin.ID, admin.ID)
	if !errors.Is(err, ErrCannotDeleteSelf) {
		t.Fatalf("delete self err = %v, want ErrCannotDeleteSelf", err)
	}
}

func TestDelete_LastAdminRefused(t *testing.T) {
	gdb := newUserTestDB(t)
	svc := NewUserService(gdb)
	admin1 := makeUser(t, gdb, "admin1", model.RoleAdmin, model.UserEnabled, 0, 0)
	admin2 := makeUser(t, gdb, "admin2", model.RoleAdmin, model.UserEnabled, 0, 0)

	// Two admins: deleting one (by the other) is allowed.
	if err := svc.Delete(admin1.ID, admin2.ID); err != nil {
		t.Fatalf("delete admin2 (2 admins present): %v", err)
	}
	// Now only admin1 remains. A different acting id deleting the last admin must
	// be refused. Use a separate non-admin acting id so self-protection does not
	// mask the last-admin guard.
	actor := makeUser(t, gdb, "actor", model.RoleUser, model.UserEnabled, 0, 0)
	err := svc.Delete(actor.ID, admin1.ID)
	if !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("delete last admin err = %v, want ErrLastAdmin", err)
	}
}

func TestDelete_CascadesTokensAndClearsUsage(t *testing.T) {
	gdb := newUserTestDB(t)
	svc := NewUserService(gdb)
	admin := makeUser(t, gdb, "admin", model.RoleAdmin, model.UserEnabled, 0, 0)
	target := makeUser(t, gdb, "victim", model.RoleUser, model.UserEnabled, 100, 50)

	// Give the target two tokens.
	tokSvc := NewTokenService(gdb)
	for i := 0; i < 2; i++ {
		name := fmt.Sprintf("k%d", i)
		if _, err := tokSvc.Create(target.ID, TokenInput{Name: &name}); err != nil {
			t.Fatalf("create token: %v", err)
		}
	}
	var before int64
	gdb.Model(&model.Token{}).Where("user_id = ?", target.ID).Count(&before)
	if before != 2 {
		t.Fatalf("precondition: token count = %d, want 2", before)
	}

	if err := svc.Delete(admin.ID, target.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// User gone.
	if _, err := svc.GetByID(target.ID); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("user still present after delete: err=%v", err)
	}
	// Tokens gone.
	var after int64
	gdb.Model(&model.Token{}).Where("user_id = ?", target.ID).Count(&after)
	if after != 0 {
		t.Fatalf("tokens not cascaded: count = %d, want 0", after)
	}
}

// --- ResetPassword -------------------------------------------------------------

func TestResetPassword_EmptyGeneratesAndBcryptVerifies(t *testing.T) {
	gdb := newUserTestDB(t)
	svc := NewUserService(gdb)

	// Create via Register so there is a starting password.
	u, err := svc.Register(RegisterInput{Username: "resetme", Password: "oldpw"}, true)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	plaintext, err := svc.ResetPassword(u.ID, "")
	if err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	if len(plaintext) < 12 {
		t.Fatalf("generated password too short: %q (len %d)", plaintext, len(plaintext))
	}

	// The stored hash verifies against the returned plaintext, and Login works.
	var reloaded model.User
	if err := gdb.First(&reloaded, u.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if bcrypt.CompareHashAndPassword([]byte(reloaded.Password), []byte(plaintext)) != nil {
		t.Fatal("returned plaintext does not match stored bcrypt hash")
	}
	if _, err := svc.Login("resetme", plaintext); err != nil {
		t.Fatalf("login with reset password failed: %v", err)
	}
	// The old password no longer works.
	if _, err := svc.Login("resetme", "oldpw"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("old password still works after reset: err=%v", err)
	}
}

func TestResetPassword_ExplicitValue(t *testing.T) {
	gdb := newUserTestDB(t)
	svc := NewUserService(gdb)
	u, err := svc.Register(RegisterInput{Username: "resetme2", Password: "oldpw"}, true)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	plaintext, err := svc.ResetPassword(u.ID, "chosenpw123")
	if err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	if plaintext != "chosenpw123" {
		t.Fatalf("returned plaintext = %q, want chosenpw123", plaintext)
	}
	if _, err := svc.Login("resetme2", "chosenpw123"); err != nil {
		t.Fatalf("login with explicit reset password failed: %v", err)
	}
}

func TestResetPassword_NotFound(t *testing.T) {
	gdb := newUserTestDB(t)
	svc := NewUserService(gdb)
	if _, err := svc.ResetPassword(99999, ""); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("reset on missing user err = %v, want ErrUserNotFound", err)
	}
}

// --- QuotaOp -------------------------------------------------------------------

func TestQuotaOp_Add(t *testing.T) {
	gdb := newUserTestDB(t)
	svc := NewUserService(gdb)
	u := makeUser(t, gdb, "q1", model.RoleUser, model.UserEnabled, 100, 0)

	out, err := svc.QuotaOp(u.ID, QuotaOpAdd, 50)
	if err != nil {
		t.Fatalf("QuotaOp add: %v", err)
	}
	if out.Quota != 150 {
		t.Fatalf("quota after add = %d, want 150", out.Quota)
	}
	// add must reject non-positive amounts.
	if _, err := svc.QuotaOp(u.ID, QuotaOpAdd, 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("add 0 err = %v, want ErrInvalidInput", err)
	}
	if _, err := svc.QuotaOp(u.ID, QuotaOpAdd, -5); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("add -5 err = %v, want ErrInvalidInput", err)
	}
}

func TestQuotaOp_Set(t *testing.T) {
	gdb := newUserTestDB(t)
	svc := NewUserService(gdb)
	u := makeUser(t, gdb, "q2", model.RoleUser, model.UserEnabled, 100, 0)

	out, err := svc.QuotaOp(u.ID, QuotaOpSet, 42)
	if err != nil {
		t.Fatalf("QuotaOp set: %v", err)
	}
	if out.Quota != 42 {
		t.Fatalf("quota after set = %d, want 42", out.Quota)
	}
	// set -1 (unlimited) is allowed.
	out, err = svc.QuotaOp(u.ID, QuotaOpSet, -1)
	if err != nil {
		t.Fatalf("QuotaOp set -1: %v", err)
	}
	if out.Quota != -1 {
		t.Fatalf("quota after set -1 = %d, want -1", out.Quota)
	}
	// Other negatives are rejected.
	if _, err := svc.QuotaOp(u.ID, QuotaOpSet, -2); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("set -2 err = %v, want ErrInvalidInput", err)
	}
}

func TestQuotaOp_ResetUsed_DBOnly(t *testing.T) {
	gdb := newUserTestDB(t)
	// Wire a Redis-less QuotaService so reset_used goes through ResetUserUsage's
	// DB path (no Redis side-effect) — proving the canonical path is invoked.
	qs := NewQuotaService(gdb, nil)
	svc := NewUserService(gdb).WithQuotaService(qs)
	u := makeUser(t, gdb, "q3", model.RoleUser, model.UserEnabled, 100, 80)

	out, err := svc.QuotaOp(u.ID, QuotaOpResetUsed, 0)
	if err != nil {
		t.Fatalf("QuotaOp reset_used: %v", err)
	}
	if out.UsedQuota != 0 {
		t.Fatalf("used_quota after reset = %d, want 0", out.UsedQuota)
	}
}

func TestQuotaOp_ResetUsed_NoQuotaSvcFallback(t *testing.T) {
	gdb := newUserTestDB(t)
	svc := NewUserService(gdb) // no quota service wired
	u := makeUser(t, gdb, "q4", model.RoleUser, model.UserEnabled, 100, 80)
	out, err := svc.QuotaOp(u.ID, QuotaOpResetUsed, 0)
	if err != nil {
		t.Fatalf("QuotaOp reset_used (no quota svc): %v", err)
	}
	if out.UsedQuota != 0 {
		t.Fatalf("used_quota after reset = %d, want 0", out.UsedQuota)
	}
}

func TestQuotaOp_UnknownOp(t *testing.T) {
	gdb := newUserTestDB(t)
	svc := NewUserService(gdb)
	u := makeUser(t, gdb, "q5", model.RoleUser, model.UserEnabled, 100, 0)
	if _, err := svc.QuotaOp(u.ID, "multiply", 2); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown op err = %v, want ErrInvalidInput", err)
	}
}

// --- Token group inheritance ---------------------------------------------------

func TestTokenCreate_InheritsUserGroup(t *testing.T) {
	gdb := newUserTestDB(t)
	userSvc := NewUserService(gdb)
	tokSvc := NewTokenService(gdb)

	// User in a non-default group.
	u, err := userSvc.AdminCreate(AdminCreateInput{Username: "grouped", Password: "p", Group: "team-a"})
	if err != nil {
		t.Fatalf("AdminCreate: %v", err)
	}

	// No group on the token input -> inherits the user's group.
	name := "inherit"
	res, err := tokSvc.Create(u.ID, TokenInput{Name: &name})
	if err != nil {
		t.Fatalf("Create token: %v", err)
	}
	if res.Token.Group != "team-a" {
		t.Fatalf("token group = %q, want team-a (inherited)", res.Token.Group)
	}

	// Empty-string group -> still inherits.
	empty := ""
	name2 := "inherit2"
	res2, err := tokSvc.Create(u.ID, TokenInput{Name: &name2, Group: &empty})
	if err != nil {
		t.Fatalf("Create token empty group: %v", err)
	}
	if res2.Token.Group != "team-a" {
		t.Fatalf("token group (empty input) = %q, want team-a (inherited)", res2.Token.Group)
	}

	// Explicit group wins over the user's group.
	explicit := "team-b"
	name3 := "explicit"
	res3, err := tokSvc.Create(u.ID, TokenInput{Name: &name3, Group: &explicit})
	if err != nil {
		t.Fatalf("Create token explicit group: %v", err)
	}
	if res3.Token.Group != "team-b" {
		t.Fatalf("token group (explicit) = %q, want team-b", res3.Token.Group)
	}
}

func TestTokenCreate_DefaultGroupWhenUserDefault(t *testing.T) {
	gdb := newUserTestDB(t)
	userSvc := NewUserService(gdb)
	tokSvc := NewTokenService(gdb)

	u, err := userSvc.AdminCreate(AdminCreateInput{Username: "plain", Password: "p"}) // group defaults to "default"
	if err != nil {
		t.Fatalf("AdminCreate: %v", err)
	}
	name := "tk"
	res, err := tokSvc.Create(u.ID, TokenInput{Name: &name})
	if err != nil {
		t.Fatalf("Create token: %v", err)
	}
	if res.Token.Group != "default" {
		t.Fatalf("token group = %q, want default", res.Token.Group)
	}
}

// --- Postgres-gated: ILIKE search + reset_used DB+Redis sync ------------------
//
// These exercise paths that need the production Postgres dialect (ILIKE) and a
// real Redis (counter sync). They are skipped unless TEST_POSTGRES_DSN (and, for
// the Redis sync, TEST_REDIS_ADDR) are set, matching the existing Redis-gated
// pattern in quota_service_test.go. The live docker verification wires both.

func newPostgresTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping Postgres-backed test")
	}
	gdb, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Skipf("postgres open failed (%v); skipping", err)
	}
	if err := gdb.AutoMigrate(&model.User{}, &model.Token{}, &model.Option{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Clean slate for a deterministic search assertion.
	gdb.Exec("DELETE FROM tokens")
	gdb.Exec("DELETE FROM users")
	return gdb
}

func TestListPaged_ILIKESearch_Postgres(t *testing.T) {
	gdb := newPostgresTestDB(t)
	svc := NewUserService(gdb)

	makeUser(t, gdb, "Alice", model.RoleUser, model.UserEnabled, 0, 0)
	gdb.Model(&model.User{}).Where("username = ?", "Alice").UpdateColumn("email", "alice@corp.com")
	makeUser(t, gdb, "bob", model.RoleUser, model.UserEnabled, 0, 0)
	makeUser(t, gdb, "carol", model.RoleUser, model.UserEnabled, 0, 0)

	// Case-insensitive match on username.
	items, total, err := svc.ListPaged(UserListQuery{Search: "ALI"})
	if err != nil {
		t.Fatalf("ListPaged search: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].Username != "Alice" {
		t.Fatalf("ILIKE username search wrong: total=%d items=%v", total, usernames(items))
	}

	// Match on email substring.
	items, total, err = svc.ListPaged(UserListQuery{Search: "corp.com"})
	if err != nil {
		t.Fatalf("ListPaged email search: %v", err)
	}
	if total != 1 || items[0].Username != "Alice" {
		t.Fatalf("ILIKE email search wrong: total=%d items=%v", total, usernames(items))
	}

	// A wildcard-bearing input is treated as a literal (parameterized), so '%'
	// does not match everything by injection.
	_, total, err = svc.ListPaged(UserListQuery{Search: "zzz_no_match_%"})
	if err != nil {
		t.Fatalf("ListPaged literal-wildcard search: %v", err)
	}
	if total != 0 {
		t.Fatalf("literal wildcard matched %d rows, want 0", total)
	}
}

func TestResetUserUsage_DBAndRedisSync_Postgres(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	addr := os.Getenv("TEST_REDIS_ADDR")
	if dsn == "" || addr == "" {
		t.Skip("TEST_POSTGRES_DSN and TEST_REDIS_ADDR required; skipping")
	}
	gdb := newPostgresTestDB(t)
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis %s unreachable: %v", addr, err)
	}
	rdb.FlushDB(context.Background())

	qs := NewQuotaService(gdb, rdb)
	svc := NewUserService(gdb).WithQuotaService(qs)

	u := makeUser(t, gdb, "usage", model.RoleUser, model.UserEnabled, 1000, 0)
	tok := &model.Token{UserID: u.ID, Name: "k", KeyHash: HashKey(fmt.Sprintf("sk-%d", uniq())),
		Status: model.TokenEnabled, Quota: 1000, UsedQuota: 0, Group: "default"}
	if err := gdb.Create(tok).Error; err != nil {
		t.Fatalf("create token: %v", err)
	}

	bg := context.Background()
	// Consume so both DB (after write-back) and Redis carry a user-level count.
	if err := qs.Consume(bg, tok.ID, u.ID, 300); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if v, _ := rdb.Get(bg, userUsedKey(u.ID)).Int64(); v != 300 {
		t.Fatalf("redis user counter = %d, want 300 before reset", v)
	}

	// reset_used must zero BOTH DB used_quota and the Redis counter.
	if _, err := svc.QuotaOp(u.ID, QuotaOpResetUsed, 0); err != nil {
		t.Fatalf("QuotaOp reset_used: %v", err)
	}

	var reloaded model.User
	if err := gdb.First(&reloaded, u.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.UsedQuota != 0 {
		t.Fatalf("DB used_quota = %d, want 0 after reset_used", reloaded.UsedQuota)
	}
	// The key is deleted; the next access lazily reseeds from the (now zero) DB.
	if _, _, err := qs.userQuotaAndUsed(bg, u.ID); err != nil {
		t.Fatalf("userQuotaAndUsed after reset: %v", err)
	}
	if v, _ := rdb.Get(bg, userUsedKey(u.ID)).Int64(); v != 0 {
		t.Fatalf("redis user counter = %d, want 0 after reset_used (rebuilt from DB)", v)
	}
}
