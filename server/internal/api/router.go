// Package api registers the Gin HTTP routes.
package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/controller"
	"github.com/agent-router/server/internal/middleware"
	"github.com/agent-router/server/internal/relay"
	"github.com/agent-router/server/internal/router"
	"github.com/agent-router/server/internal/service"
)

// Deps bundles the dependencies needed by route handlers.
type Deps struct {
	DB        *gorm.DB
	Redis     *redis.Client
	JWTSecret string // HS256 signing secret for admin JWTs (Tech Design §4)
	SecretKey string // AES-GCM key material for channel key encryption (Tech Design §11)

	// Quota optionally supplies a shared QuotaService so the relay reuses the
	// same instance whose write-back loop main() runs. When nil a fresh
	// instance (backed by the same DB/Redis) is constructed; quota counters live
	// in Redis keyed by entity id, so either instance is correct.
	Quota *service.QuotaService
}

// New builds the Gin engine with base middleware and foundational routes.
// Downstream tasks (T3-T8) extend the returned engine with their route groups.
func New(d Deps) *gin.Engine {
	r := gin.New()
	r.Use(middleware.Recover())
	r.Use(middleware.Logger())
	r.Use(middleware.CORS())

	apiGroup := r.Group("/api")
	apiGroup.GET("/ping", pingHandler(d))

	registerAuthRoutes(apiGroup, d)
	registerChannelRoutes(apiGroup, d)
	registerTokenRoutes(apiGroup, d)
	registerRuleRoutes(apiGroup, d)
	registerDashboardRoutes(apiGroup, d)

	registerRelayRoutes(r, d)

	return r
}

// registerRelayRoutes wires up the T7 public relay gateway (Tech Design §4/§6/§8
// Relay). The endpoints live under /v1 (not /api) and authenticate with a
// downstream sk- API key (or Anthropic x-api-key) plus a pre-flight quota check,
// then route the request to an upstream channel, adapting the response back to
// the inbound format. All errors are rendered in the matching inbound schema.
func registerRelayRoutes(r *gin.Engine, d Deps) {
	if d.DB == nil {
		return
	}

	tokenSvc := service.NewTokenService(d.DB)
	channelSvc := service.NewChannelService(d.DB, d.Redis, d.SecretKey)
	logSvc := service.NewLogService(d.DB)
	engine := router.NewEngine(d.DB)

	quotaSvc := d.Quota
	if quotaSvc == nil {
		quotaSvc = service.NewQuotaService(d.DB, d.Redis)
	}

	pricingSvc := service.NewPricingService(d.DB)
	relayer := relay.NewRelayer(engine, channelSvc, quotaSvc, logSvc, pricingSvc, d.DB)
	relayCtrl := controller.NewRelayController(relayer, channelSvc)

	v1 := r.Group("/v1")
	v1.Use(middleware.RelayAuth(tokenSvc, d.DB))

	// Models listing needs auth but not the quota guard.
	v1.GET("/models", relayCtrl.Models)

	// Inference endpoints additionally run the pre-flight quota check.
	infer := v1.Group("")
	infer.Use(middleware.Quota(quotaSvc))
	infer.POST("/chat/completions", relayCtrl.ChatCompletions)
	infer.POST("/messages", relayCtrl.Messages)
}

// registerRuleRoutes wires up the T5 routing-rule management endpoints
// (Tech Design §8). All rule routes require a valid admin JWT.
func registerRuleRoutes(api *gin.RouterGroup, d Deps) {
	if d.DB == nil {
		return
	}

	ruleSvc := service.NewRuleService(d.DB)
	ruleCtrl := controller.NewRuleController(ruleSvc)

	admin := api.Group("")
	admin.Use(middleware.JWTAuth(d.JWTSecret), middleware.AdminOnly())
	admin.GET("/rules", ruleCtrl.List)
	admin.GET("/rules/:id", ruleCtrl.Get)
	admin.POST("/rules", ruleCtrl.Create)
	admin.PUT("/rules/:id", ruleCtrl.Update)
	admin.DELETE("/rules/:id", ruleCtrl.Delete)
}

// registerDashboardRoutes wires up the T8 request-log analytics endpoints
// (Tech Design §8): the dashboard summary/timeseries aggregations and the
// paginated log listing. All routes require a valid JWT; the controller scopes
// every query to the caller's own data for non-admins and grants full
// visibility (with an optional user filter) to admins.
func registerDashboardRoutes(api *gin.RouterGroup, d Deps) {
	if d.DB == nil {
		return
	}

	logSvc := service.NewLogService(d.DB)
	dashCtrl := controller.NewDashboardController(logSvc)

	auth := api.Group("")
	auth.Use(middleware.JWTAuth(d.JWTSecret))
	auth.GET("/dashboard/summary", dashCtrl.Summary)
	auth.GET("/dashboard/timeseries", dashCtrl.Timeseries)
	auth.GET("/logs", dashCtrl.Logs)
}

// registerTokenRoutes wires up the T4 downstream API-key (token) management
// endpoints (Tech Design §8). All token routes require a valid JWT and are
// scoped to the authenticated user: a user manages only their own keys.
func registerTokenRoutes(api *gin.RouterGroup, d Deps) {
	if d.DB == nil {
		return
	}

	tokenSvc := service.NewTokenService(d.DB)
	tokenCtrl := controller.NewTokenController(tokenSvc)

	auth := api.Group("")
	auth.Use(middleware.JWTAuth(d.JWTSecret))
	auth.GET("/tokens", tokenCtrl.List)
	auth.GET("/tokens/:id", tokenCtrl.Get)
	auth.POST("/tokens", tokenCtrl.Create)
	auth.PUT("/tokens/:id", tokenCtrl.Update)
	auth.DELETE("/tokens/:id", tokenCtrl.Delete)
}

// registerChannelRoutes wires up the T3 channel-management endpoints
// (Tech Design §8). All channel routes require a valid admin JWT.
func registerChannelRoutes(api *gin.RouterGroup, d Deps) {
	if d.DB == nil {
		return
	}

	channelSvc := service.NewChannelService(d.DB, d.Redis, d.SecretKey)
	channelCtrl := controller.NewChannelController(channelSvc)

	// testChatCtrl serves the admin direct test-chat path (Tech Design §3). It is
	// mounted on the SAME admin group as the other /api/channels routes but is
	// deliberately NOT placed behind the Quota middleware and never selects via
	// the routing engine or consumes quota. It DOES write a single is_test
	// request_log per attempt (token_id=nil), so the LogService is injected here
	// (api.Deps carries no LogService — construct one locally, the same pattern
	// as registerRelayRoutes/registerDashboardRoutes).
	logSvc := service.NewLogService(d.DB)
	pricingSvc := service.NewPricingService(d.DB)
	testChatCtrl := relay.NewTestChatController(channelSvc, logSvc, pricingSvc)
	pricingCtrl := controller.NewPricingController(pricingSvc)

	admin := api.Group("")
	admin.Use(middleware.JWTAuth(d.JWTSecret), middleware.AdminOnly())
	admin.GET("/channels", channelCtrl.List)
	admin.GET("/channels/:id", channelCtrl.Get)
	admin.POST("/channels", channelCtrl.Create)
	admin.PUT("/channels/:id", channelCtrl.Update)
	admin.DELETE("/channels/:id", channelCtrl.Delete)
	admin.POST("/channels/:id/fetch-models", channelCtrl.FetchModels)
	admin.POST("/channels/:id/test", channelCtrl.Test)
	admin.POST("/channels/:id/test-chat", testChatCtrl.TestChat)

	// Model pricing (USD billing) — same admin group.
	admin.GET("/pricing", pricingCtrl.List)
	admin.PUT("/pricing", pricingCtrl.Upsert)
	admin.DELETE("/pricing/:id", pricingCtrl.Delete)
}

// registerAuthRoutes wires up the T2 auth, setup and user-management endpoints
// (Tech Design §8).
func registerAuthRoutes(api *gin.RouterGroup, d Deps) {
	if d.DB == nil {
		return
	}

	// Wire the shared QuotaService into the UserService so admin reset_used and
	// user deletion clear the user-level Redis counter through the single
	// canonical path. Reuse d.Quota when supplied (same instance whose write-back
	// loop main() drives); otherwise construct one over the same DB/Redis.
	quotaSvc := d.Quota
	if quotaSvc == nil {
		quotaSvc = service.NewQuotaService(d.DB, d.Redis)
	}
	userSvc := service.NewUserService(d.DB).WithQuotaService(quotaSvc)
	authCtrl := controller.NewAuthController(userSvc, d.JWTSecret)
	userCtrl := controller.NewUserController(userSvc)

	// Public endpoints.
	api.POST("/auth/register", authCtrl.Register)
	api.POST("/auth/login", authCtrl.Login)
	api.GET("/setup/status", authCtrl.SetupStatus)

	// Authenticated self-service endpoints.
	auth := api.Group("")
	auth.Use(middleware.JWTAuth(d.JWTSecret))
	auth.GET("/user/self", userCtrl.GetSelf)
	auth.PUT("/user/self", userCtrl.UpdateSelf)

	// Admin-only user management.
	admin := api.Group("")
	admin.Use(middleware.JWTAuth(d.JWTSecret), middleware.AdminOnly())
	admin.GET("/users", userCtrl.List)
	admin.POST("/users", userCtrl.Create)
	admin.PUT("/users/:id", userCtrl.AdminUpdate)
	admin.DELETE("/users/:id", userCtrl.Delete)
	admin.POST("/users/:id/reset-password", userCtrl.ResetPassword)
	admin.POST("/users/:id/quota", userCtrl.QuotaOp)
}

// pingHandler reports liveness and the health of DB and Redis connections.
func pingHandler(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		dbOK, redisOK := true, true

		if d.DB != nil {
			if sqlDB, err := d.DB.DB(); err != nil || sqlDB.PingContext(c.Request.Context()) != nil {
				dbOK = false
			}
		} else {
			dbOK = false
		}

		if d.Redis != nil {
			ctx, cancel := context.WithCancel(c.Request.Context())
			defer cancel()
			if err := d.Redis.Ping(ctx).Err(); err != nil {
				redisOK = false
			}
		} else {
			redisOK = false
		}

		status := http.StatusOK
		if !dbOK || !redisOK {
			status = http.StatusServiceUnavailable
		}
		c.JSON(status, gin.H{
			"message": "pong",
			"db":      dbOK,
			"redis":   redisOK,
		})
	}
}
