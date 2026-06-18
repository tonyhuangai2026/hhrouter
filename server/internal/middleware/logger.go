package middleware

import (
	"log"
	"time"

	"github.com/gin-gonic/gin"
)

// Logger returns structured request logging middleware. It records method,
// path, status, latency and client IP after each request. Authorization
// material is never logged.
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		raw := c.Request.URL.RawQuery

		c.Next()

		latency := time.Since(start)
		status := c.Writer.Status()
		if raw != "" {
			path = path + "?" + raw
		}
		log.Printf("[gin] %3d | %13v | %-15s | %-7s %s",
			status, latency, c.ClientIP(), c.Request.Method, path)
	}
}
