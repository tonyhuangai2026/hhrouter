package controller

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/agent-router/server/internal/middleware"
	"github.com/agent-router/server/internal/model"
	"github.com/agent-router/server/internal/relay"
)

// RelayController hosts the public relay endpoints (Tech Design §8 Relay):
// POST /v1/chat/completions, POST /v1/messages and GET /v1/models. The chat and
// messages handlers delegate to the relay.Relayer (which owns routing, the
// upstream call, failover, usage accounting and logging); the models handler
// aggregates the models the authenticated key may use.
type RelayController struct {
	relayer *relay.Relayer
	lister  ChannelLister
}

// ChannelLister abstracts listing enabled channels for the /v1/models
// aggregation, so the controller does not depend on a concrete service or *gorm.DB.
type ChannelLister interface {
	EnabledChannels() ([]model.Channel, error)
}

// NewRelayController constructs a RelayController. channels lists the enabled
// channels used to build the /v1/models aggregation.
func NewRelayController(relayer *relay.Relayer, channels ChannelLister) *RelayController {
	return &RelayController{relayer: relayer, lister: channels}
}

// ChatCompletions handles POST /v1/chat/completions.
func (rc *RelayController) ChatCompletions(c *gin.Context) {
	rc.relayer.HandleChatCompletions(c)
}

// Messages handles POST /v1/messages.
func (rc *RelayController) Messages(c *gin.Context) {
	rc.relayer.HandleMessages(c)
}

// Models handles GET /v1/models — the aggregate list of models the authenticated
// key may use. The response follows the OpenAI list shape ({object:list,data:[]})
// which both OpenAI and Anthropic SDKs tolerate for a model listing. The set is
// the union of all enabled channels' models (including model_mapping external
// names), intersected with the token's allowed_models restriction when present.
func (rc *RelayController) Models(c *gin.Context) {
	tok, ok := middleware.CurrentRelayToken(c)
	if !ok {
		relay.WriteClassError(c, relay.FormatOpenAI, http.StatusUnauthorized, relay.ClassAuthentication, "authentication required")
		return
	}

	channels, err := rc.lister.EnabledChannels()
	if err != nil {
		relay.WriteClassError(c, relay.FormatOpenAI, http.StatusInternalServerError, relay.ClassInternal, "could not list models")
		return
	}

	allowed := decodeAllowed(tok)
	seen := map[string]struct{}{}
	for i := range channels {
		for _, m := range channelModelNames(&channels[i]) {
			if _, dup := seen[m]; dup {
				continue
			}
			if allowed != nil {
				if _, ok := allowed[m]; !ok {
					continue
				}
			}
			seen[m] = struct{}{}
		}
	}

	ids := make([]string, 0, len(seen))
	for m := range seen {
		ids = append(ids, m)
	}
	sort.Strings(ids)

	data := make([]gin.H, 0, len(ids))
	created := time.Now().Unix()
	for _, id := range ids {
		data = append(data, gin.H{
			"id":       id,
			"object":   "model",
			"created":  created,
			"owned_by": "agent-router",
		})
	}
	c.JSON(http.StatusOK, gin.H{"object": "list", "data": data})
}

// channelModelNames returns the externally-usable model names a channel serves:
// its models list plus any model_mapping keys (external names).
func channelModelNames(ch *model.Channel) []string {
	var out []string
	if len(ch.Models) > 0 {
		var models []string
		if json.Unmarshal(ch.Models, &models) == nil {
			out = append(out, models...)
		}
	}
	if len(ch.ModelMapping) > 0 {
		var mapping map[string]string
		if json.Unmarshal(ch.ModelMapping, &mapping) == nil {
			for ext := range mapping {
				out = append(out, ext)
			}
		}
	}
	return out
}

// decodeAllowed returns the token's allowed-model set, or nil for "no
// restriction" (empty/absent list).
func decodeAllowed(tok *model.Token) map[string]struct{} {
	if len(tok.AllowedModels) == 0 {
		return nil
	}
	var list []string
	if json.Unmarshal(tok.AllowedModels, &list) != nil || len(list) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(list))
	for _, m := range list {
		set[m] = struct{}{}
	}
	return set
}
