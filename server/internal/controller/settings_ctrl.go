package controller

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/model"
)

// SettingsController exposes miscellaneous admin toggles that don't warrant their
// own resource. Currently: request-log I/O capture (record each request's input
// and output onto the log row).
type SettingsController struct {
	db *gorm.DB
}

// NewSettingsController constructs the controller.
func NewSettingsController(db *gorm.DB) *SettingsController {
	return &SettingsController{db: db}
}

type settingsBody struct {
	// LogIO: when true, the relay records request input/output onto request_logs.
	LogIO bool `json:"log_io"`
}

// Get handles GET /api/settings.
func (c *SettingsController) Get(ctx *gin.Context) {
	ctx.JSON(http.StatusOK, settingsBody{
		LogIO: model.GetOption(c.db, model.OptRequestLogIO, "false") == "true",
	})
}

// Put handles PUT /api/settings.
func (c *SettingsController) Put(ctx *gin.Context) {
	var in settingsBody
	if err := ctx.ShouldBindJSON(&in); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "invalid body", "type": "invalid_request"}})
		return
	}
	val := "false"
	if in.LogIO {
		val = "true"
	}
	if err := model.SetOption(c.db, model.OptRequestLogIO, val); err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "failed to save settings", "type": "internal"}})
		return
	}
	ctx.JSON(http.StatusOK, in)
}
