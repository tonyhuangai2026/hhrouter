// Package service holds the business-logic layer sitting between controllers
// and the data/model layer.
package service

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/model"
)

// Sentinel errors returned by UserService so controllers can map them to the
// right HTTP status without leaking internals.
var (
	ErrUserExists        = errors.New("username already taken")
	ErrInvalidCredential = errors.New("invalid username or password")
	ErrUserDisabled      = errors.New("account is disabled")
	ErrUserNotFound      = errors.New("user not found")
	ErrRegisterDisabled  = errors.New("registration is disabled")
	ErrInvalidInput      = errors.New("invalid input")
	// ErrCannotDeleteSelf is returned when an admin attempts to delete their own
	// account (self-protection, Tech Design §2.3 / §3).
	ErrCannotDeleteSelf = errors.New("cannot delete your own account")
	// ErrLastAdmin is returned when deleting the target would leave the system
	// with no admin accounts.
	ErrLastAdmin = errors.New("cannot delete the last admin account")
)

// userUsageResetter is the minimal surface UserService needs from QuotaService
// to keep user-level usage consistent across DB and the Redis counter when an
// admin resets used quota or deletes a user. QuotaService.ResetUserUsage
// satisfies it. Optional: when nil, UserService falls back to a direct DB
// update (used_quota=0) with no Redis side-effect.
type userUsageResetter interface {
	ResetUserUsage(ctx context.Context, userID uint) error
}

// UserService implements registration, login verification and user CRUD on top
// of GORM (Tech Design §4, §8).
type UserService struct {
	db *gorm.DB
	// quota is an optional collaborator used to keep user-level usage counters
	// consistent (DB + Redis) on reset_used and delete. It may be nil, in which
	// case those paths fall back to a direct DB update with no Redis effect.
	quota userUsageResetter
}

// NewUserService constructs a UserService.
func NewUserService(db *gorm.DB) *UserService {
	return &UserService{db: db}
}

// WithQuotaService wires a QuotaService (or any userUsageResetter) so reset_used
// and user deletion can clear the user-level Redis counter through the single
// canonical path rather than hand-rolling a DEL. Returns the receiver for
// fluent construction. Passing nil is allowed (disables the Redis side-effect).
func (s *UserService) WithQuotaService(q userUsageResetter) *UserService {
	s.quota = q
	return s
}

// Count returns the number of users in the system. Used by the setup/status
// endpoint and to decide the first-user-is-admin bootstrap rule.
func (s *UserService) Count() (int64, error) {
	var n int64
	if err := s.db.Model(&model.User{}).Count(&n).Error; err != nil {
		return 0, err
	}
	return n, nil
}

// RegisterInput carries the fields accepted on self-service registration.
type RegisterInput struct {
	Username    string
	Password    string
	DisplayName string
	Email       string
}

// Register creates a new account. The very first user in an empty system is
// promoted to admin; all subsequent users get the regular user role. When
// fromAdmin is false the RegisterEnabled option is enforced (admins may always
// create accounts). The returned user has its password hash cleared via the
// model's json:"-" tag, but callers should still avoid leaking it.
func (s *UserService) Register(in RegisterInput, fromAdmin bool) (*model.User, error) {
	in.Username = strings.TrimSpace(in.Username)
	if in.Username == "" || in.Password == "" {
		return nil, ErrInvalidInput
	}

	var created *model.User
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&model.User{}).Count(&count).Error; err != nil {
			return err
		}

		// Enforce the RegisterEnabled option for self-service registration only
		// when at least one user already exists (the first-ever account is the
		// bootstrap admin and must always be allowed).
		if !fromAdmin && count > 0 {
			if !registerEnabled(tx) {
				return ErrRegisterDisabled
			}
		}

		var existing int64
		if err := tx.Model(&model.User{}).Where("username = ?", in.Username).Count(&existing).Error; err != nil {
			return err
		}
		if existing > 0 {
			return ErrUserExists
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
		if err != nil {
			return err
		}

		role := model.RoleUser
		if count == 0 {
			role = model.RoleAdmin
		}

		u := &model.User{
			Username:    in.Username,
			Password:    string(hash),
			DisplayName: in.DisplayName,
			Email:       in.Email,
			Role:        role,
			Status:      model.UserEnabled,
			Quota:       model.DefaultUserQuota(tx),
		}
		if err := tx.Create(u).Error; err != nil {
			return err
		}
		created = u
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// Login verifies the username/password pair and returns the user on success.
// It returns ErrInvalidCredential for unknown users or wrong passwords (without
// distinguishing the two) and ErrUserDisabled for disabled accounts.
func (s *UserService) Login(username, password string) (*model.User, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return nil, ErrInvalidCredential
	}

	var u model.User
	err := s.db.Where("username = ?", username).First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrInvalidCredential
	}
	if err != nil {
		return nil, err
	}

	if bcrypt.CompareHashAndPassword([]byte(u.Password), []byte(password)) != nil {
		return nil, ErrInvalidCredential
	}
	if u.Status != model.UserEnabled {
		return nil, ErrUserDisabled
	}

	// Best-effort: record the successful login time. A failure here (e.g. a
	// transient DB hiccup) must never block an otherwise-valid login, so the
	// error is intentionally ignored. UpdateColumn skips hooks/UpdatedAt churn.
	now := time.Now()
	if err := s.db.Model(&model.User{}).Where("id = ?", u.ID).
		UpdateColumn("last_login_at", now).Error; err == nil {
		u.LastLoginAt = &now
	}

	return &u, nil
}

// GetByID loads a single user by primary key.
func (s *UserService) GetByID(id uint) (*model.User, error) {
	var u model.User
	err := s.db.First(&u, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// List returns all users ordered by ID (admin only).
func (s *UserService) List() ([]model.User, error) {
	var users []model.User
	if err := s.db.Order("id asc").Find(&users).Error; err != nil {
		return nil, err
	}
	return users, nil
}

// SelfUpdateInput carries the fields a user may change on their own profile.
type SelfUpdateInput struct {
	DisplayName *string
	Email       *string
	Password    *string
}

// UpdateSelf applies a self-service profile update. Only non-nil fields change.
func (s *UserService) UpdateSelf(id uint, in SelfUpdateInput) (*model.User, error) {
	u, err := s.GetByID(id)
	if err != nil {
		return nil, err
	}

	updates := map[string]any{}
	if in.DisplayName != nil {
		updates["display_name"] = *in.DisplayName
	}
	if in.Email != nil {
		updates["email"] = *in.Email
	}
	if in.Password != nil {
		if *in.Password == "" {
			return nil, ErrInvalidInput
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(*in.Password), bcrypt.DefaultCost)
		if err != nil {
			return nil, err
		}
		updates["password"] = string(hash)
	}

	if len(updates) == 0 {
		return u, nil
	}
	if err := s.db.Model(u).Updates(updates).Error; err != nil {
		return nil, err
	}
	return s.GetByID(id)
}

// AdminUpdateInput carries the fields an admin may change on any user.
type AdminUpdateInput struct {
	DisplayName *string
	Email       *string
	Role        *model.UserRole
	Status      *model.UserStatus
	Quota       *int64
	Password    *string
	Group       *string
}

// AdminUpdate applies an admin-initiated update to the target user, supporting
// status, quota and role changes (Tech Design §8).
func (s *UserService) AdminUpdate(id uint, in AdminUpdateInput) (*model.User, error) {
	u, err := s.GetByID(id)
	if err != nil {
		return nil, err
	}

	updates := map[string]any{}
	if in.DisplayName != nil {
		updates["display_name"] = *in.DisplayName
	}
	if in.Email != nil {
		updates["email"] = *in.Email
	}
	if in.Role != nil {
		if *in.Role != model.RoleAdmin && *in.Role != model.RoleUser {
			return nil, ErrInvalidInput
		}
		updates["role"] = *in.Role
	}
	if in.Status != nil {
		if *in.Status != model.UserEnabled && *in.Status != model.UserDisabled {
			return nil, ErrInvalidInput
		}
		updates["status"] = *in.Status
	}
	if in.Quota != nil {
		updates["quota"] = *in.Quota
	}
	if in.Group != nil {
		g := strings.TrimSpace(*in.Group)
		if g == "" {
			g = "default"
		}
		updates["group"] = g
	}
	if in.Password != nil {
		if *in.Password == "" {
			return nil, ErrInvalidInput
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(*in.Password), bcrypt.DefaultCost)
		if err != nil {
			return nil, err
		}
		updates["password"] = string(hash)
	}

	if len(updates) == 0 {
		return u, nil
	}
	if err := s.db.Model(u).Updates(updates).Error; err != nil {
		return nil, err
	}
	return s.GetByID(id)
}

// UserListQuery parameterizes the paginated, filterable user listing
// (Tech Design §2.1). Zero values fall back to documented defaults.
type UserListQuery struct {
	Page     int    // 1-based; <=0 -> 1
	PageSize int    // <=0 -> 20; capped at maxUserPageSize
	Search   string // case-insensitive substring match on username/email
	Role     string // exact-match filter; "" = any
	Status   string // exact-match filter; "" = any
	Sort     string // whitelist: created_at|used_quota|username (default created_at)
	Order    string // asc|desc (default desc)
}

// maxUserPageSize caps the page size so an admin cannot request an unbounded
// result set (Tech Design §2.1).
const maxUserPageSize = 100

// userSortColumns is the whitelist of sortable columns. ORDER BY is built only
// from these constant identifiers — a caller-supplied Sort never reaches SQL
// verbatim, defeating ORDER BY injection (Tech Design §2.1 / §6).
var userSortColumns = map[string]string{
	"created_at": "created_at",
	"used_quota": "used_quota",
	"username":   "username",
}

// ListPaged returns a page of users matching the query plus the total count of
// matching rows (ignoring pagination). Search is applied via parameterized
// ILIKE so user input is never interpolated into SQL; sort/order are resolved
// through a column whitelist with safe defaults.
func (s *UserService) ListPaged(q UserListQuery) (items []model.User, total int64, err error) {
	page := q.Page
	if page <= 0 {
		page = 1
	}
	pageSize := q.PageSize
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > maxUserPageSize {
		pageSize = maxUserPageSize
	}

	sortCol, ok := userSortColumns[q.Sort]
	if !ok {
		sortCol = "created_at"
	}
	order := "desc"
	if strings.EqualFold(q.Order, "asc") {
		order = "asc"
	}

	// Build the base query with all filters; reused for both Count and Find so
	// total reflects the same predicate as the page.
	base := s.db.Model(&model.User{})
	if search := strings.TrimSpace(q.Search); search != "" {
		// ILIKE with a parameterized %term% pattern: the value is bound, never
		// concatenated, so wildcards/quotes in the input are treated as data.
		pattern := "%" + search + "%"
		base = base.Where("username ILIKE ? OR email ILIKE ?", pattern, pattern)
	}
	if q.Role != "" {
		base = base.Where("role = ?", q.Role)
	}
	if q.Status != "" {
		base = base.Where("status = ?", q.Status)
	}

	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	offset := (page - 1) * pageSize
	// sortCol and order are both drawn from constant sets above, so this Order
	// string contains no user-controlled substring.
	if err := base.
		Order(sortCol + " " + order).
		Limit(pageSize).
		Offset(offset).
		Find(&items).Error; err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// AdminCreateInput carries every field an admin may set when creating a user.
// Unlike RegisterInput it can carry role/status/quota/group; pointer-free
// fields use documented defaults when empty (NOTE-1 resolution).
type AdminCreateInput struct {
	Username    string
	Password    string
	DisplayName string
	Email       string
	Role        string // "" -> user
	Status      string // "" -> enabled
	Quota       *int64 // nil -> DefaultUserQuota
	Group       string // "" -> default
}

// AdminCreate creates a user with admin-supplied role/status/quota/group set in
// the same transaction (NOTE-1). RegisterInput cannot carry those fields and
// Register hardcodes them, so this is a dedicated path rather than a thin reuse
// of Register: it performs the same uniqueness check + bcrypt hashing, then
// persists every field atomically. A username collision returns ErrUserExists
// (mapped to 409 by the controller).
func (s *UserService) AdminCreate(in AdminCreateInput) (*model.User, error) {
	in.Username = strings.TrimSpace(in.Username)
	if in.Username == "" {
		return nil, ErrInvalidInput
	}

	// Resolve role/status with validation and defaults.
	role := model.RoleUser
	if in.Role != "" {
		role = model.UserRole(in.Role)
		if role != model.RoleAdmin && role != model.RoleUser {
			return nil, ErrInvalidInput
		}
	}
	status := model.UserEnabled
	if in.Status != "" {
		status = model.UserStatus(in.Status)
		if status != model.UserEnabled && status != model.UserDisabled {
			return nil, ErrInvalidInput
		}
	}
	group := strings.TrimSpace(in.Group)
	if group == "" {
		group = "default"
	}

	// An admin-created account may omit the password; generate a strong temp one
	// so the row is never left with an unusable/empty hash. (The plaintext is not
	// surfaced here — admins use the reset-password endpoint to obtain one. The
	// controller may also require password explicitly; keeping a generated
	// fallback avoids an invalid bcrypt hash.)
	password := in.Password
	if password == "" {
		gen, err := generateTempPassword()
		if err != nil {
			return nil, err
		}
		password = gen
	}

	var created *model.User
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var existing int64
		if err := tx.Model(&model.User{}).Where("username = ?", in.Username).Count(&existing).Error; err != nil {
			return err
		}
		if existing > 0 {
			return ErrUserExists
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return err
		}

		quota := model.DefaultUserQuota(tx)
		if in.Quota != nil {
			quota = *in.Quota
		}

		u := &model.User{
			Username:    in.Username,
			Password:    string(hash),
			DisplayName: in.DisplayName,
			Email:       in.Email,
			Role:        role,
			Status:      status,
			Quota:       quota,
			Group:       group,
		}
		if err := tx.Create(u).Error; err != nil {
			return err
		}
		created = u
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// Delete removes a user and their tokens transactionally, with self-protection
// and a last-admin guard (Tech Design §2.3):
//   - targetID == actingUID -> ErrCannotDeleteSelf
//   - target is admin and admin count <= 1 -> ErrLastAdmin
//
// After the DB transaction commits it best-effort clears the user-level Redis
// usage counter through the quota service (a failure there does not fail the
// delete — the user row is already gone).
func (s *UserService) Delete(actingUID, targetID uint) error {
	if targetID == actingUID {
		return ErrCannotDeleteSelf
	}

	target, err := s.GetByID(targetID)
	if err != nil {
		return err
	}

	err = s.db.Transaction(func(tx *gorm.DB) error {
		if target.Role == model.RoleAdmin {
			var admins int64
			if err := tx.Model(&model.User{}).Where("role = ?", model.RoleAdmin).Count(&admins).Error; err != nil {
				return err
			}
			if admins <= 1 {
				return ErrLastAdmin
			}
		}
		// Remove the user's tokens first (FK-free schema, but we keep the order
		// explicit so no orphan tokens survive), then the user.
		if err := tx.Where("user_id = ?", targetID).Delete(&model.Token{}).Error; err != nil {
			return err
		}
		if err := tx.Delete(&model.User{}, targetID).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Best-effort: clear the user-level Redis counter via the canonical path so
	// a recreated user with the same id does not inherit a stale count. The row
	// is already deleted; ResetUserUsage's DB update is a harmless no-op.
	if s.quota != nil {
		_ = s.quota.ResetUserUsage(context.Background(), targetID)
	}
	return nil
}

// ResetPassword sets a new password for the target user, returning the plaintext
// exactly once for the controller to surface (Tech Design §2.4). When
// newPassword is empty a strong random temporary password is generated. The
// stored value is always a bcrypt hash.
func (s *UserService) ResetPassword(targetID uint, newPassword string) (plaintext string, err error) {
	if _, err := s.GetByID(targetID); err != nil {
		return "", err
	}

	plaintext = newPassword
	if plaintext == "" {
		gen, err := generateTempPassword()
		if err != nil {
			return "", err
		}
		plaintext = gen
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	if err := s.db.Model(&model.User{}).Where("id = ?", targetID).
		UpdateColumn("password", string(hash)).Error; err != nil {
		return "", err
	}
	return plaintext, nil
}

// Quota operation names accepted by QuotaOp.
const (
	QuotaOpAdd       = "add"
	QuotaOpSet       = "set"
	QuotaOpResetUsed = "reset_used"
)

// QuotaOp mutates a user's quota or used-quota (Tech Design §2.5):
//   - add:        quota += amount (amount must be > 0)
//   - set:        quota  = amount (amount may be -1 to mean unlimited)
//   - reset_used: used_quota = 0 AND clear the user-level Redis counter via the
//     quota service (ResetUserUsage), keeping DB and cache consistent.
//
// The DB mutation runs in a transaction; for reset_used the Redis clear happens
// after commit through the canonical quota-service path (never a hand-rolled
// DEL elsewhere).
func (s *UserService) QuotaOp(targetID uint, op string, amount int64) (*model.User, error) {
	if _, err := s.GetByID(targetID); err != nil {
		return nil, err
	}

	switch op {
	case QuotaOpAdd:
		if amount <= 0 {
			return nil, ErrInvalidInput
		}
		if err := s.db.Model(&model.User{}).Where("id = ?", targetID).
			UpdateColumn("quota", gorm.Expr("quota + ?", amount)).Error; err != nil {
			return nil, err
		}
	case QuotaOpSet:
		// amount may be -1 (unlimited); any other negative is invalid.
		if amount < -1 {
			return nil, ErrInvalidInput
		}
		if err := s.db.Model(&model.User{}).Where("id = ?", targetID).
			UpdateColumn("quota", amount).Error; err != nil {
			return nil, err
		}
	case QuotaOpResetUsed:
		if s.quota != nil {
			// Canonical path: zeroes DB used_quota AND the Redis counter.
			if err := s.quota.ResetUserUsage(context.Background(), targetID); err != nil {
				return nil, err
			}
		} else {
			// No quota service wired (e.g. unit tests without Redis): zero DB only.
			if err := s.db.Model(&model.User{}).Where("id = ?", targetID).
				UpdateColumn("used_quota", 0).Error; err != nil {
				return nil, err
			}
		}
	default:
		return nil, ErrInvalidInput
	}

	return s.GetByID(targetID)
}

// tempPasswordLength is the length of a generated temporary password. 20
// characters from a 70+ symbol alphabet gives ample entropy for a one-time
// admin-issued credential the user is expected to change.
const tempPasswordLength = 20

// tempPasswordAlphabet intentionally spans lower/upper/digits/punctuation so a
// generated password satisfies common complexity expectations. It excludes
// visually ambiguous characters is not required here; the value is shown once
// and copyable.
const tempPasswordAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$%^&*-_=+"

// generateTempPassword returns a cryptographically-random password drawn
// uniformly from tempPasswordAlphabet using crypto/rand.
func generateTempPassword() (string, error) {
	b := make([]byte, tempPasswordLength)
	max := big.NewInt(int64(len(tempPasswordAlphabet)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = tempPasswordAlphabet[n.Int64()]
	}
	return string(b), nil
}

// registerEnabled reads the RegisterEnabled option (defaults to enabled when the
// option is missing or unparsable).
func registerEnabled(db *gorm.DB) bool {
	v := strings.ToLower(strings.TrimSpace(model.GetOption(db, model.OptRegisterEnabled, "true")))
	return v != "false" && v != "0" && v != "off" && v != "no"
}
