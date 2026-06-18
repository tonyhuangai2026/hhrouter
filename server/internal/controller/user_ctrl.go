package controller

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/agent-router/server/internal/middleware"
	"github.com/agent-router/server/internal/model"
	"github.com/agent-router/server/internal/service"
)

// UserController handles self-service profile endpoints and the admin user
// management endpoints (Tech Design §8).
type UserController struct {
	users *service.UserService
}

// NewUserController constructs a UserController.
func NewUserController(users *service.UserService) *UserController {
	return &UserController{users: users}
}

// GetSelf handles GET /api/user/self, returning the authenticated user.
func (u *UserController) GetSelf(c *gin.Context) {
	id, ok := middleware.CurrentUserID(c)
	if !ok {
		respondError(c, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	usr, err := u.users.GetByID(id)
	if err != nil {
		u.respondLookupError(c, err)
		return
	}
	c.JSON(http.StatusOK, usr)
}

type selfUpdateRequest struct {
	DisplayName *string `json:"display_name"`
	Email       *string `json:"email"`
	Password    *string `json:"password"`
}

// UpdateSelf handles PUT /api/user/self.
func (u *UserController) UpdateSelf(c *gin.Context) {
	id, ok := middleware.CurrentUserID(c)
	if !ok {
		respondError(c, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	var req selfUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	usr, err := u.users.UpdateSelf(id, service.SelfUpdateInput{
		DisplayName: req.DisplayName,
		Email:       req.Email,
		Password:    req.Password,
	})
	if err != nil {
		u.respondLookupError(c, err)
		return
	}
	c.JSON(http.StatusOK, usr)
}

// UserView is the admin-facing serialization of a user. It deliberately never
// includes the password hash (the model already has json:"-", but a dedicated
// DTO makes the safe shape explicit and decoupled from the model). Fields mirror
// Tech Design §3: id, username, display_name, email, role, status, quota,
// used_quota, group, last_login_at, created_at.
type UserView struct {
	ID          uint             `json:"id"`
	Username    string           `json:"username"`
	DisplayName string           `json:"display_name"`
	Email       string           `json:"email,omitempty"`
	Role        model.UserRole   `json:"role"`
	Status      model.UserStatus `json:"status"`
	Quota       int64            `json:"quota"`
	UsedQuota   int64            `json:"used_quota"`
	Group       string           `json:"group"`
	LastLoginAt *time.Time       `json:"last_login_at,omitempty"`
	CreatedAt   time.Time        `json:"created_at"`
}

// toUserView projects a model.User onto the password-free DTO.
func toUserView(u *model.User) UserView {
	return UserView{
		ID:          u.ID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Email:       u.Email,
		Role:        u.Role,
		Status:      u.Status,
		Quota:       u.Quota,
		UsedQuota:   u.UsedQuota,
		Group:       u.Group,
		LastLoginAt: u.LastLoginAt,
		CreatedAt:   u.CreatedAt,
	}
}

// List handles GET /api/users (admin only). It is paginated and filterable:
// query params page, page_size, search (ILIKE on username/email), role, status,
// sort (created_at|used_quota|username) and order (asc|desc). The response is
// {items, total, page, page_size} where items are password-free UserViews.
func (u *UserController) List(c *gin.Context) {
	page := parseIntDefault(c.Query("page"), 1)
	pageSize := parseIntDefault(c.Query("page_size"), 20)

	users, total, err := u.users.ListPaged(service.UserListQuery{
		Page:     page,
		PageSize: pageSize,
		Search:   c.Query("search"),
		Role:     c.Query("role"),
		Status:   c.Query("status"),
		Sort:     c.Query("sort"),
		Order:    c.Query("order"),
	})
	if err != nil {
		respondError(c, http.StatusInternalServerError, "internal_error", "could not list users")
		return
	}

	items := make([]UserView, 0, len(users))
	for i := range users {
		items = append(items, toUserView(&users[i]))
	}
	c.JSON(http.StatusOK, gin.H{
		"items":     items,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

type adminUpdateRequest struct {
	DisplayName *string `json:"display_name"`
	Email       *string `json:"email"`
	Role        *string `json:"role"`
	Status      *string `json:"status"`
	Quota       *int64  `json:"quota"`
	Password    *string `json:"password"`
	Group       *string `json:"group"`
}

// AdminUpdate handles PUT /api/users/:id (admin only) — change status, quota,
// role, group and other fields of any user. Self-protection: an admin may not
// disable or demote (admin->user) their own account (Tech Design §3); those are
// refused with 409 before reaching the service.
func (u *UserController) AdminUpdate(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid user id")
		return
	}

	var req adminUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}

	// Self-protection: refuse to disable or demote the acting admin's own
	// account. Editing other fields on yourself (display name, email, quota,
	// password, group) remains allowed.
	if actingID, ok := middleware.CurrentUserID(c); ok && actingID == uint(id) {
		if req.Status != nil && model.UserStatus(*req.Status) == model.UserDisabled {
			respondError(c, http.StatusConflict, "conflict", "you cannot disable your own account")
			return
		}
		if req.Role != nil && model.UserRole(*req.Role) != model.RoleAdmin {
			respondError(c, http.StatusConflict, "conflict", "you cannot remove your own admin role")
			return
		}
	}

	in := service.AdminUpdateInput{
		DisplayName: req.DisplayName,
		Email:       req.Email,
		Quota:       req.Quota,
		Password:    req.Password,
		Group:       req.Group,
	}
	if req.Role != nil {
		role := model.UserRole(*req.Role)
		in.Role = &role
	}
	if req.Status != nil {
		status := model.UserStatus(*req.Status)
		in.Status = &status
	}

	usr, err := u.users.AdminUpdate(uint(id), in)
	if err != nil {
		u.respondLookupError(c, err)
		return
	}
	c.JSON(http.StatusOK, toUserView(usr))
}

type adminCreateRequest struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	Role        string `json:"role"`
	Status      string `json:"status"`
	Quota       *int64 `json:"quota"`
	Group       string `json:"group"`
}

// Create handles POST /api/users (admin only). It creates a user with the given
// role/status/quota/group via the service AdminCreate path. A username conflict
// returns 409.
func (u *UserController) Create(c *gin.Context) {
	var req adminCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}

	usr, err := u.users.AdminCreate(service.AdminCreateInput{
		Username:    req.Username,
		Password:    req.Password,
		DisplayName: req.DisplayName,
		Email:       req.Email,
		Role:        req.Role,
		Status:      req.Status,
		Quota:       req.Quota,
		Group:       req.Group,
	})
	if err != nil {
		u.respondLookupError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toUserView(usr))
}

// Delete handles DELETE /api/users/:id (admin only). The acting admin's id is
// passed through so the service can refuse self-deletion and last-admin
// deletion. The user's tokens are removed in the same transaction.
func (u *UserController) Delete(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid user id")
		return
	}
	actingID, ok := middleware.CurrentUserID(c)
	if !ok {
		respondError(c, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}

	if err := u.users.Delete(actingID, uint(id)); err != nil {
		u.respondLookupError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

type resetPasswordRequest struct {
	Password *string `json:"password"`
}

// ResetPassword handles POST /api/users/:id/reset-password (admin only). The
// optional body password is used verbatim; when absent/empty a strong random
// temporary password is generated. The plaintext is returned exactly once.
func (u *UserController) ResetPassword(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid user id")
		return
	}

	var req resetPasswordRequest
	// An empty body is valid (means "generate one"); only reject malformed JSON.
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			respondError(c, http.StatusBadRequest, "invalid_request", "invalid request body")
			return
		}
	}

	newPassword := ""
	if req.Password != nil {
		newPassword = *req.Password
	}

	plaintext, err := u.users.ResetPassword(uint(id), newPassword)
	if err != nil {
		u.respondLookupError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"password": plaintext})
}

type quotaOpRequest struct {
	Op     string `json:"op"`
	Amount int64  `json:"amount"`
}

// QuotaOp handles POST /api/users/:id/quota (admin only): body {op, amount}
// where op is add | set | reset_used.
func (u *UserController) QuotaOp(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid user id")
		return
	}

	var req quotaOpRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}

	usr, err := u.users.QuotaOp(uint(id), req.Op, req.Amount)
	if err != nil {
		u.respondLookupError(c, err)
		return
	}
	c.JSON(http.StatusOK, toUserView(usr))
}

func (u *UserController) respondLookupError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrUserNotFound):
		respondError(c, http.StatusNotFound, "not_found", "user not found")
	case errors.Is(err, service.ErrUserExists):
		respondError(c, http.StatusConflict, "conflict", "username already taken")
	case errors.Is(err, service.ErrCannotDeleteSelf):
		respondError(c, http.StatusConflict, "conflict", "you cannot delete your own account")
	case errors.Is(err, service.ErrLastAdmin):
		respondError(c, http.StatusConflict, "conflict", "cannot delete the last admin account")
	case errors.Is(err, service.ErrInvalidInput):
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid input")
	default:
		respondError(c, http.StatusInternalServerError, "internal_error", "could not process request")
	}
}
