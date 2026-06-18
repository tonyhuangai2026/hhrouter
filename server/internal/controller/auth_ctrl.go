// Package controller holds the Gin HTTP handlers for the admin API surface
// (Tech Design §8).
package controller

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/agent-router/server/internal/middleware"
	"github.com/agent-router/server/internal/model"
	"github.com/agent-router/server/internal/service"
)

// AuthController handles registration, login and the first-deploy setup probe.
type AuthController struct {
	users     *service.UserService
	jwtSecret string
}

// NewAuthController constructs an AuthController.
func NewAuthController(users *service.UserService, jwtSecret string) *AuthController {
	return &AuthController{users: users, jwtSecret: jwtSecret}
}

type registerRequest struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
}

type authResponse struct {
	Token string      `json:"token"`
	User  *model.User `json:"user"`
}

// Register handles POST /api/auth/register. The first user in an empty system
// becomes an admin; otherwise the RegisterEnabled option must be on.
func (a *AuthController) Register(c *gin.Context) {
	var req registerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}

	u, err := a.users.Register(service.RegisterInput{
		Username:    req.Username,
		Password:    req.Password,
		DisplayName: req.DisplayName,
		Email:       req.Email,
	}, false)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrUserExists):
			respondError(c, http.StatusConflict, "conflict", "username already taken")
		case errors.Is(err, service.ErrRegisterDisabled):
			respondError(c, http.StatusForbidden, "forbidden", "registration is disabled")
		case errors.Is(err, service.ErrInvalidInput):
			respondError(c, http.StatusBadRequest, "invalid_request", "username and password are required")
		default:
			respondError(c, http.StatusInternalServerError, "internal_error", "could not create user")
		}
		return
	}

	token, err := middleware.IssueToken(a.jwtSecret, u.ID, u.Role)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "internal_error", "could not issue token")
		return
	}
	c.JSON(http.StatusCreated, authResponse{Token: token, User: u})
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Login handles POST /api/auth/login. On success it returns a signed JWT.
func (a *AuthController) Login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}

	u, err := a.users.Login(req.Username, req.Password)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidCredential):
			respondError(c, http.StatusUnauthorized, "unauthorized", "invalid username or password")
		case errors.Is(err, service.ErrUserDisabled):
			respondError(c, http.StatusForbidden, "forbidden", "account is disabled")
		default:
			respondError(c, http.StatusInternalServerError, "internal_error", "could not authenticate")
		}
		return
	}

	token, err := middleware.IssueToken(a.jwtSecret, u.ID, u.Role)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "internal_error", "could not issue token")
		return
	}
	c.JSON(http.StatusOK, authResponse{Token: token, User: u})
}

// SetupStatus handles GET /api/setup/status, reporting whether any users exist
// so the SPA can drive the first-deploy admin bootstrap flow (Tech Design §9).
func (a *AuthController) SetupStatus(c *gin.Context) {
	count, err := a.users.Count()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "internal_error", "could not read system status")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"initialized": count > 0,
		"user_count":  count,
		"has_users":   count > 0,
	})
}

// respondError writes an OpenAI-style structured error (Tech Design §11).
func respondError(c *gin.Context, status int, errType, message string) {
	c.AbortWithStatusJSON(status, gin.H{
		"error": gin.H{
			"message": message,
			"type":    errType,
		},
	})
}
