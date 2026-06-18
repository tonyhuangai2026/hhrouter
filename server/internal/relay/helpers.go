package relay

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"

	"github.com/agent-router/server/internal/model"
)

// newID returns a short random hex id used to stamp response ids (chatcmpl-/msg_).
func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000000000000000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// decodeAllowedModels parses a token's allowed_models JSONB array into a string
// slice. A nil/empty/invalid value yields an empty slice (no restriction).
func decodeAllowedModels(tok *model.Token) []string {
	if len(tok.AllowedModels) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(tok.AllowedModels, &out); err != nil {
		return nil
	}
	return out
}
