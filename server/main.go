// Command server is the Agent Router Platform backend entrypoint.
//
// Boot sequence: load config -> connect PostgreSQL & Redis -> AutoMigrate ->
// seed default options and optional bootstrap admin -> start Gin (with the
// /api/ping health check). Connection failures abort startup.
package main

import (
	"log"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/agent-router/server/config"
	"github.com/agent-router/server/internal/api"
	"github.com/agent-router/server/internal/db"
	"github.com/agent-router/server/internal/service"
)

// quotaWriteBackInterval is how often the two-level quota counters in Redis are
// flushed to the durable DB used_quota columns (Tech Design §4).
const quotaWriteBackInterval = 30 * time.Second

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("startup: %v", err)
	}

	gin.SetMode(cfg.GinMode)

	gdb, err := db.Connect(cfg.DBDSN)
	if err != nil {
		log.Fatalf("startup: connect database: %v", err)
	}

	rdb, err := db.ConnectRedis(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
	if err != nil {
		log.Fatalf("startup: connect redis: %v", err)
	}
	defer func() { _ = rdb.Close() }()

	if err := db.AutoMigrate(gdb); err != nil {
		log.Fatalf("startup: migrate: %v", err)
	}

	if err := db.SeedDefaultOptions(gdb); err != nil {
		log.Fatalf("startup: seed options: %v", err)
	}

	if cfg.HasBootstrapAdmin() {
		created, err := db.SeedAdmin(gdb, cfg.AdminUsername, cfg.AdminPassword)
		if err != nil {
			log.Fatalf("startup: seed admin: %v", err)
		}
		if created {
			log.Printf("startup: seeded bootstrap admin %q", cfg.AdminUsername)
		}
	}

	// Start the two-level quota write-back loop so Redis counters are
	// periodically persisted to DB used_quota. The same QuotaService is reused
	// by the relay (T7) for CheckRemaining/Consume.
	quotaSvc := service.NewQuotaService(gdb, rdb)
	quotaSvc.StartWriteBack(quotaWriteBackInterval)
	defer quotaSvc.StopWriteBack()

	r := api.New(api.Deps{DB: gdb, Redis: rdb, JWTSecret: cfg.JWTSecret, SecretKey: cfg.SecretKey, Quota: quotaSvc})

	addr := ":" + cfg.Port
	log.Printf("startup: listening on %s", addr)
	if err := r.Run(addr); err != nil {
		log.Fatalf("startup: server exited: %v", err)
	}
}
