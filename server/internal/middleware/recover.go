package middleware

import (
	"log"
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"
)

// Recover returns middleware that recovers from panics, logs the stack, and
// returns a 500 JSON error instead of crashing the server.
func Recover() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("[panic] %v\n%s", err, debug.Stack())
				if !c.Writer.Written() {
					c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
						"error": gin.H{
							"message": "internal server error",
							"type":    "internal_error",
						},
					})
				}
			}
		}()
		c.Next()
	}
}
