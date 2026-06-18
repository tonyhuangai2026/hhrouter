package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/crypto"
	"github.com/agent-router/server/internal/model"
)

// Channel-related sentinel errors.
var (
	ErrChannelNotFound = errors.New("channel not found")
	ErrInvalidChannel  = errors.New("invalid channel input")
)

// modelFetchTimeout bounds upstream calls for fetch-models / connectivity test.
const modelFetchTimeout = 15 * time.Second

// modelCacheTTL is how long an OpenAI channel's fetched model list is cached in
// Redis (Tech Design §7: 10 minutes).
const modelCacheTTL = 10 * time.Minute

// bedrockBuiltinModels is the built-in list of common Bedrock model ids
// surfaced for bedrock channels (Tech Design §7). Operators may edit a
// channel's models afterwards to add/remove ids manually.
var bedrockBuiltinModels = []string{
	"anthropic.claude-3-5-sonnet-20240620-v1:0",
	"anthropic.claude-3-5-sonnet-20241022-v2:0",
	"anthropic.claude-3-5-haiku-20241022-v1:0",
	"anthropic.claude-3-haiku-20240307-v1:0",
	"anthropic.claude-3-opus-20240229-v1:0",
	"us.anthropic.claude-3-5-sonnet-20241022-v2:0",
	"us.anthropic.claude-sonnet-4-20250514-v1:0",
	"us.anthropic.claude-opus-4-20250514-v1:0",
	"amazon.nova-pro-v1:0",
	"amazon.nova-lite-v1:0",
	"amazon.nova-micro-v1:0",
	"meta.llama3-1-70b-instruct-v1:0",
	"meta.llama3-1-8b-instruct-v1:0",
	"meta.llama3-3-70b-instruct-v1:0",
}

// BedrockBuiltinModels returns a copy of the built-in Bedrock model id list.
func BedrockBuiltinModels() []string {
	out := make([]string, len(bedrockBuiltinModels))
	copy(out, bedrockBuiltinModels)
	return out
}

// ChannelService implements upstream channel CRUD plus model auto-fetch and
// connectivity testing (Tech Design §3 channels, §7, §8). The upstream key is
// AES-GCM encrypted at rest using secretKey and only ever returned masked.
type ChannelService struct {
	db        *gorm.DB
	rdb       *redis.Client
	secretKey string
	http      *http.Client
}

// NewChannelService constructs a ChannelService. rdb may be nil (model-fetch
// caching is then skipped); http defaults to a client with a sane timeout.
func NewChannelService(db *gorm.DB, rdb *redis.Client, secretKey string) *ChannelService {
	return &ChannelService{
		db:        db,
		rdb:       rdb,
		secretKey: secretKey,
		http:      &http.Client{Timeout: modelFetchTimeout},
	}
}

// ChannelInput carries the writable fields of a channel. Pointer fields on
// update mean "leave unchanged when nil"; on create, nil falls back to defaults.
type ChannelInput struct {
	Name         *string
	Type         *model.ChannelType
	BaseURL      *string
	Key          *string // plaintext; encrypted before persistence. nil = keep existing
	Region       *string
	Models       *[]string
	ModelMapping *map[string]string
	Group        *string
	Priority     *int
	Weight       *int
	Status       *model.ChannelStatus
	// UseInferenceProfile is a pointer so create can distinguish "unset" (nil →
	// let GORM default:true apply) from an explicit false; update applies the
	// explicit value (including false).
	UseInferenceProfile *bool
}

// ChannelView is the outward representation of a channel: the same fields as the
// model but with the secret key replaced by a non-reversible mask and a boolean
// indicating whether a key is configured.
type ChannelView struct {
	model.Channel
	KeyMasked string `json:"key_masked"`
	HasKey    bool   `json:"has_key"`
}

// toView builds a masked view of a stored channel. The encrypted key is
// decrypted only to compute the display mask and never returned in plaintext.
func (s *ChannelService) toView(ch *model.Channel) ChannelView {
	v := ChannelView{Channel: *ch}
	v.Channel.Key = "" // ensure the (encrypted) key never leaks via embedding
	if ch.Key != "" {
		v.HasKey = true
		if plain, err := crypto.Decrypt(s.secretKey, ch.Key); err == nil {
			v.KeyMasked = crypto.Mask(plain)
		} else {
			v.KeyMasked = "****"
		}
	}
	return v
}

// Decrypt returns the plaintext upstream key for a channel, decrypting the
// stored AES-GCM ciphertext. Downstream tasks (T6 adapters) use this to obtain
// the credential at request time. An empty stored key yields an empty string.
func (s *ChannelService) Decrypt(ch *model.Channel) (string, error) {
	return crypto.Decrypt(s.secretKey, ch.Key)
}

// Create validates and persists a new channel, encrypting the key at rest.
func (s *ChannelService) Create(in ChannelInput) (*ChannelView, error) {
	ch := &model.Channel{
		Group:    "default",
		Priority: 0,
		Weight:   1,
		Status:   model.ChannelEnabled,
	}

	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidChannel)
	}
	if in.Type == nil {
		return nil, fmt.Errorf("%w: type is required", ErrInvalidChannel)
	}
	if !validChannelType(*in.Type) {
		return nil, fmt.Errorf("%w: type must be openai, bedrock, or anthropic", ErrInvalidChannel)
	}

	s.applyInput(ch, in)

	// Encrypt key (nil/empty -> empty string).
	keyPlain := ""
	if in.Key != nil {
		keyPlain = *in.Key
	}
	enc, err := crypto.Encrypt(s.secretKey, keyPlain)
	if err != nil {
		return nil, err
	}
	ch.Key = enc

	if ch.Models == nil {
		ch.Models = datatypes.JSON([]byte("[]"))
	}

	// GORM substitutes the column DEFAULT for any field left at its Go zero
	// value when the field has a `default` tag. UseInferenceProfile defaults to
	// true, so an explicitly-supplied false would otherwise be silently turned
	// into true on insert. When the caller set the flag explicitly (pointer
	// non-nil), force GORM to write every column with Select("*") so a literal
	// false is persisted; when nil, the plain Create lets the DB default (true)
	// apply.
	if err := s.db.Create(ch).Error; err != nil {
		return nil, err
	}
	// GORM substitutes the column DEFAULT for any field left at its Go zero value
	// when the field carries a `default` tag. UseInferenceProfile defaults to
	// true, so an explicitly-supplied false is silently turned into true on
	// insert. When the caller set the flag explicitly (pointer non-nil), write
	// the literal value back so a false sticks; when nil, the insert above keeps
	// the DB default (true).
	if in.UseInferenceProfile != nil && ch.UseInferenceProfile != *in.UseInferenceProfile {
		if err := s.db.Model(ch).Update("use_inference_profile", *in.UseInferenceProfile).Error; err != nil {
			return nil, err
		}
		ch.UseInferenceProfile = *in.UseInferenceProfile
	}
	v := s.toView(ch)
	return &v, nil
}

// Update applies a partial update to an existing channel. Only non-nil input
// fields change. The key is re-encrypted only when a new key is supplied.
func (s *ChannelService) Update(id uint, in ChannelInput) (*ChannelView, error) {
	ch, err := s.getRaw(id)
	if err != nil {
		return nil, err
	}

	if in.Type != nil && !validChannelType(*in.Type) {
		return nil, fmt.Errorf("%w: type must be openai, bedrock, or anthropic", ErrInvalidChannel)
	}
	if in.Name != nil && strings.TrimSpace(*in.Name) == "" {
		return nil, fmt.Errorf("%w: name cannot be empty", ErrInvalidChannel)
	}

	s.applyInput(ch, in)

	if in.Key != nil {
		enc, err := crypto.Encrypt(s.secretKey, *in.Key)
		if err != nil {
			return nil, err
		}
		ch.Key = enc
	}

	if err := s.db.Save(ch).Error; err != nil {
		return nil, err
	}
	v := s.toView(ch)
	return &v, nil
}

// applyInput copies non-nil scalar/JSON fields from in onto ch (excluding Key,
// which is handled separately because it needs encryption).
func (s *ChannelService) applyInput(ch *model.Channel, in ChannelInput) {
	if in.Name != nil {
		ch.Name = strings.TrimSpace(*in.Name)
	}
	if in.Type != nil {
		ch.Type = *in.Type
	}
	if in.BaseURL != nil {
		ch.BaseURL = strings.TrimRight(strings.TrimSpace(*in.BaseURL), "/")
	}
	if in.Region != nil {
		ch.Region = strings.TrimSpace(*in.Region)
	}
	if in.Models != nil {
		ch.Models = toJSONArray(*in.Models)
	}
	if in.ModelMapping != nil {
		if b, err := json.Marshal(*in.ModelMapping); err == nil {
			ch.ModelMapping = datatypes.JSON(b)
		}
	}
	if in.Group != nil {
		g := strings.TrimSpace(*in.Group)
		if g == "" {
			g = "default"
		}
		ch.Group = g
	}
	if in.Priority != nil {
		ch.Priority = *in.Priority
	}
	if in.Weight != nil {
		ch.Weight = *in.Weight
	}
	if in.Status != nil {
		ch.Status = *in.Status
	}
	// Only set the column when the pointer is non-nil. On create a nil pointer
	// leaves the field at its zero value (false) so GORM's default:true applies;
	// on update the explicit value (including false) takes effect.
	if in.UseInferenceProfile != nil {
		ch.UseInferenceProfile = *in.UseInferenceProfile
	}
}

// Delete removes a channel by id, cascading to its model_prices rows in the same
// transaction (there is no DB-level FK — the codebase keeps cross-table cleanup
// explicit). The channel delete and the price cleanup either both commit or both
// roll back, so a deleted channel never leaves orphan price rows behind.
func (s *ChannelService) Delete(id uint) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		res := tx.Delete(&model.Channel{}, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrChannelNotFound
		}
		// Cascade: remove this channel's price rows. Deleting zero rows (the
		// channel had no prices) is fine.
		if err := tx.Where("channel_id = ?", id).Delete(&model.ModelPrice{}).Error; err != nil {
			return err
		}
		return nil
	})
}

// Get returns a single channel as a masked view.
func (s *ChannelService) Get(id uint) (*ChannelView, error) {
	ch, err := s.getRaw(id)
	if err != nil {
		return nil, err
	}
	v := s.toView(ch)
	return &v, nil
}

// List returns all channels (masked) ordered by id.
func (s *ChannelService) List() ([]ChannelView, error) {
	var chs []model.Channel
	if err := s.db.Order("id asc").Find(&chs).Error; err != nil {
		return nil, err
	}
	out := make([]ChannelView, 0, len(chs))
	for i := range chs {
		out = append(out, s.toView(&chs[i]))
	}
	return out, nil
}

// EnabledChannels returns all enabled channels (raw models) for relay-side
// aggregation such as the /v1/models listing (T7). Unlike List it does not mask
// keys and is scoped to enabled channels only.
func (s *ChannelService) EnabledChannels() ([]model.Channel, error) {
	var chs []model.Channel
	if err := s.db.Where("status = ?", model.ChannelEnabled).Order("id asc").Find(&chs).Error; err != nil {
		return nil, err
	}
	return chs, nil
}

// getRaw loads a channel including its encrypted key (for internal use).
func (s *ChannelService) getRaw(id uint) (*model.Channel, error) {
	var ch model.Channel
	err := s.db.First(&ch, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrChannelNotFound
	}
	if err != nil {
		return nil, err
	}
	return &ch, nil
}

// GetRaw loads a single channel including its encrypted key. It is the exported
// counterpart of getRaw, used by the admin direct test-chat path (Tech Design
// §3) which must construct an upstream request for a specific channel without
// going through the routing engine. Returns ErrChannelNotFound when absent. The
// returned *model.Channel still carries the encrypted key; pair it with Decrypt
// (the adapter.Decryptor) to build an upstream request.
func (s *ChannelService) GetRaw(id uint) (*model.Channel, error) {
	return s.getRaw(id)
}

// FetchModelsResult reports the outcome of a model auto-fetch.
//
// Source is one of:
//   - "upstream": a successful live fetch from the provider (OpenAI /models or
//     the Bedrock control plane). This is the same value for both providers; we
//     intentionally do NOT introduce a separate "api" value (see Tech Design §1
//     reviewer note: reconcile with the existing enum rather than renaming).
//   - "cache":    served from the warm Redis cache without hitting the upstream.
//   - "builtin":  a fallback list (currently only Bedrock) used when the live
//     fetch is unavailable. When Source=="builtin" Message explains why.
//
// Message is optional and only populated for the builtin fallback path to
// convey the real-vs-fallback distinction to the caller/UI.
type FetchModelsResult struct {
	Models  []string `json:"models"`
	Cached  bool     `json:"cached"`
	Source  string   `json:"source"`            // "upstream" | "cache" | "builtin"
	Message string   `json:"message,omitempty"` // why a fallback was used, if any
}

// FetchModels populates a channel's available model list (Tech Design §1, §7).
//   - openai: GET {base_url}/v1/models (falling back to {base_url}/models on
//     404/400) with the channel key, parse data[].id, persist to channel.models
//     and cache in Redis for 10 minutes. A fresh cache hit is returned without
//     re-hitting the upstream.
//   - bedrock: GET the control-plane foundation-models listing with the bearer
//     key; on any auth/network/parse failure, fall back to the built-in common
//     model id list (Source="builtin", Message explains the fallback).
//
// refresh=true skips the Redis cache and forces a live re-fetch.
func (s *ChannelService) FetchModels(ctx context.Context, id uint, refresh bool) (*FetchModelsResult, error) {
	ch, err := s.getRaw(id)
	if err != nil {
		return nil, err
	}

	cacheKey := fmt.Sprintf("channel:models:%d", ch.ID)

	// Serve from the warm Redis cache unless a refresh was requested.
	if !refresh && s.rdb != nil {
		if cached, err := s.rdb.Get(ctx, cacheKey).Result(); err == nil && cached != "" {
			var models []string
			if json.Unmarshal([]byte(cached), &models) == nil {
				return &FetchModelsResult{Models: models, Cached: true, Source: "cache"}, nil
			}
		}
	}

	switch ch.Type {
	case model.ChannelBedrock:
		models, fallbackMsg := s.fetchBedrockModels(ctx, ch)
		if err := s.persistModels(ch, models); err != nil {
			return nil, err
		}
		s.cacheModels(ctx, cacheKey, models)
		if fallbackMsg != "" {
			return &FetchModelsResult{Models: models, Source: "builtin", Message: fallbackMsg}, nil
		}
		return &FetchModelsResult{Models: models, Source: "upstream"}, nil

	case model.ChannelAnthropic:
		models, fallbackMsg := s.fetchAnthropicModels(ctx, ch)
		if err := s.persistModels(ch, models); err != nil {
			return nil, err
		}
		s.cacheModels(ctx, cacheKey, models)
		if fallbackMsg != "" {
			return &FetchModelsResult{Models: models, Source: "builtin", Message: fallbackMsg}, nil
		}
		return &FetchModelsResult{Models: models, Source: "upstream"}, nil

	default: // openai
		models, err := s.fetchOpenAIModels(ctx, ch)
		if err != nil {
			// Do NOT overwrite the existing channel.models with an empty list on
			// a failed live fetch; surface the upstream status+body summary.
			return nil, err
		}
		if err := s.persistModels(ch, models); err != nil {
			return nil, err
		}
		s.cacheModels(ctx, cacheKey, models)
		return &FetchModelsResult{Models: models, Source: "upstream"}, nil
	}
}

// cacheModels best-effort writes the fetched model list into Redis (10m TTL).
func (s *ChannelService) cacheModels(ctx context.Context, cacheKey string, models []string) {
	if s.rdb == nil {
		return
	}
	if b, err := json.Marshal(models); err == nil {
		_ = s.rdb.Set(ctx, cacheKey, b, modelCacheTTL).Err()
	}
}

// fetchOpenAIModels calls GET {base_url}/v1/models and parses data[].id. If that
// path returns 404/400 (some OpenAI-compatible upstreams mount /models without
// the /v1 prefix) it retries GET {base_url}/models with the /v1 stripped from
// the base. On any other non-2xx status, or if both attempts fail, it returns an
// error carrying the upstream status + body summary; the caller leaves the
// existing channel.models untouched in that case.
func (s *ChannelService) fetchOpenAIModels(ctx context.Context, ch *model.Channel) ([]string, error) {
	base := strings.TrimRight(ch.BaseURL, "/")
	if base == "" {
		return nil, fmt.Errorf("%w: base_url is required for openai channels", ErrInvalidChannel)
	}
	// Normalize a base that already carries the /v1 suffix so we never produce
	// /v1/v1/models; the canonical base is the root and we add /v1 ourselves.
	base = strings.TrimRight(strings.TrimSuffix(base, "/v1"), "/")
	key, err := s.Decrypt(ch)
	if err != nil {
		return nil, fmt.Errorf("decrypt key: %w", err)
	}

	models, status, err := s.tryOpenAIModelsURL(ctx, base+"/v1/models", key)
	if err == nil {
		return models, nil
	}
	// Retry without the /v1 prefix when the first attempt was a 404/400.
	if status == http.StatusNotFound || status == http.StatusBadRequest {
		models, _, err2 := s.tryOpenAIModelsURL(ctx, base+"/models", key)
		if err2 == nil {
			return models, nil
		}
		// Surface the fallback attempt's failure (most relevant).
		return nil, err2
	}
	return nil, err
}

// tryOpenAIModelsURL issues a single GET and parses data[].id. It returns the
// parsed models, the HTTP status (0 on transport error), and an error describing
// any non-2xx response or parse failure.
func (s *ChannelService) tryOpenAIModelsURL(ctx context.Context, url, key string) ([]string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request upstream models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("upstream returned %d: %s", resp.StatusCode, snippet(body))
	}

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse models response: %w", err)
	}
	models := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models, resp.StatusCode, nil
}

// anthropicBuiltinModels is the fallback Claude model list used when the live
// {base_url}/v1/models fetch fails (network/non-2xx/parse).
var anthropicBuiltinModels = []string{
	"claude-opus-4-8",
	"claude-sonnet-4-6",
	"claude-haiku-4-5",
}

// fetchAnthropicModels calls GET {base_url}/v1/models with the x-api-key +
// anthropic-version headers and parses data[].id. On any failure (missing
// base_url is returned as an error; network/non-2xx/parse degrade gracefully) it
// falls back to the built-in Claude list and returns a non-empty fallback
// message — mirroring fetchBedrockModels' (models, fallbackMsg) contract so the
// UI still gets useful suggestions.
func (s *ChannelService) fetchAnthropicModels(ctx context.Context, ch *model.Channel) (models []string, fallbackMsg string) {
	const fallbackReason = "Anthropic /v1/models unavailable or credential rejected; falling back to the built-in Claude model list."

	base := strings.TrimRight(ch.BaseURL, "/")
	if base == "" {
		return anthropicBuiltinModels, "base_url is required to query Anthropic models; " + fallbackReason
	}
	key, err := s.Decrypt(ch)
	if err != nil {
		return anthropicBuiltinModels, "could not decrypt channel key; " + fallbackReason
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return anthropicBuiltinModels, fallbackReason
	}
	req.Header.Set("anthropic-version", "2023-06-01")
	if key != "" {
		req.Header.Set("x-api-key", key)
	}

	resp, err := s.http.Do(req)
	if err != nil {
		return anthropicBuiltinModels, fallbackReason
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return anthropicBuiltinModels, fallbackReason
	}

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return anthropicBuiltinModels, fallbackReason
	}
	out := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	if len(out) == 0 {
		return anthropicBuiltinModels, fallbackReason
	}
	return out, ""
}

// fetchBedrockModels attempts a live model listing from the Bedrock control
// plane and falls back to the built-in list on any failure.
//
// It calls GET https://bedrock.{region}.amazonaws.com/foundation-models with
// Authorization: Bearer {key} and Accept: application/json, parsing
// modelSummaries[].modelId. Models are kept when they are text/multimodal
// capable (outputModalities contains TEXT); when outputModalities is absent the
// model is kept.
//
// CAVEAT (Tech Design §1 reviewer note): AWS's ListFoundationModels control-plane
// API normally requires SigV4 request signing. A Bedrock *bearer* API key may NOT
// be accepted on bedrock.{region}.amazonaws.com/foundation-models — in practice
// this call may return 401/403. That is expected and handled here: on
// 401/403/404, any network error, or a parse failure we fall back to
// bedrockBuiltinModels and return a non-empty fallback message explaining why.
// The endpoint never fails in this scenario; it degrades to the builtin list.
//
// It returns the model list and a fallback message. An empty fallback message
// means the live fetch succeeded (Source should be "upstream"); a non-empty
// message means the builtin list was used (Source should be "builtin").
func (s *ChannelService) fetchBedrockModels(ctx context.Context, ch *model.Channel) (models []string, fallbackMsg string) {
	const fallbackReason = "Bedrock control plane unavailable or bearer credential not accepted for ListFoundationModels (it normally requires SigV4); falling back to the built-in model list."

	if ch.Region == "" {
		return BedrockBuiltinModels(), "region is required to query the Bedrock control plane; " + fallbackReason
	}
	key, err := s.Decrypt(ch)
	if err != nil {
		return BedrockBuiltinModels(), fallbackReason
	}

	url := fmt.Sprintf("https://bedrock.%s.amazonaws.com/foundation-models", ch.Region)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return BedrockBuiltinModels(), fallbackReason
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return BedrockBuiltinModels(), fallbackReason
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 401/403/404 (and any other non-2xx) → builtin fallback.
		return BedrockBuiltinModels(), fmt.Sprintf("Bedrock control plane returned %d: %s; %s", resp.StatusCode, snippet(body), fallbackReason)
	}

	var parsed struct {
		ModelSummaries []struct {
			ModelID          string   `json:"modelId"`
			OutputModalities []string `json:"outputModalities"`
		} `json:"modelSummaries"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return BedrockBuiltinModels(), fallbackReason
	}

	out := make([]string, 0, len(parsed.ModelSummaries))
	for _, m := range parsed.ModelSummaries {
		if m.ModelID == "" {
			continue
		}
		// Keep text/multimodal-capable models. When outputModalities is absent
		// (nil/empty), keep the model.
		if len(m.OutputModalities) > 0 && !containsFold(m.OutputModalities, "TEXT") {
			continue
		}
		out = append(out, m.ModelID)
	}
	// A successful but empty response is unusual; fall back so operators still
	// get a usable list rather than an empty one.
	if len(out) == 0 {
		return BedrockBuiltinModels(), "Bedrock control plane returned no usable text/multimodal models; " + fallbackReason
	}
	return out, ""
}

// containsFold reports whether items contains target (case-insensitive).
func containsFold(items []string, target string) bool {
	for _, it := range items {
		if strings.EqualFold(it, target) {
			return true
		}
	}
	return false
}

// persistModels writes the model id list onto the channel row.
func (s *ChannelService) persistModels(ch *model.Channel, models []string) error {
	ch.Models = toJSONArray(models)
	return s.db.Model(ch).Update("models", ch.Models).Error
}

// TestResult reports the outcome of a connectivity test.
type TestResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Latency int64  `json:"latency_ms"`
}

// TestChannel performs a minimal upstream request to verify connectivity
// (Tech Design §8). For openai it issues a tiny chat/completions request; for
// bedrock a minimal Converse request. It returns a structured result rather
// than an error for upstream-side failures so callers can surface a readable
// message; only internal failures (e.g. channel not found) return an error.
func (s *ChannelService) TestChannel(ctx context.Context, id uint) (*TestResult, error) {
	ch, err := s.getRaw(id)
	if err != nil {
		return nil, err
	}
	key, err := s.Decrypt(ch)
	if err != nil {
		return nil, fmt.Errorf("decrypt key: %w", err)
	}

	start := time.Now()
	var testErr error
	switch ch.Type {
	case model.ChannelOpenAI:
		testErr = s.testOpenAI(ctx, ch, key)
	case model.ChannelBedrock:
		testErr = s.testBedrock(ctx, ch, key)
	case model.ChannelAnthropic:
		testErr = s.testAnthropic(ctx, ch, key)
	default:
		testErr = fmt.Errorf("unsupported channel type %q", ch.Type)
	}
	latency := time.Since(start).Milliseconds()

	if testErr != nil {
		return &TestResult{Success: false, Message: testErr.Error(), Latency: latency}, nil
	}
	return &TestResult{Success: true, Message: "ok", Latency: latency}, nil
}

// pickTestModel returns a model id to use for the connectivity probe: the
// channel's first configured model, else a sensible default per type.
func pickTestModel(ch *model.Channel, fallback string) string {
	var models []string
	if len(ch.Models) > 0 {
		_ = json.Unmarshal(ch.Models, &models)
	}
	if len(models) > 0 {
		return models[0]
	}
	return fallback
}

// testOpenAI sends a minimal chat/completions request to {base_url}.
func (s *ChannelService) testOpenAI(ctx context.Context, ch *model.Channel, key string) error {
	base := strings.TrimRight(ch.BaseURL, "/")
	if base == "" {
		return fmt.Errorf("base_url is required for openai channels")
	}
	reqBody := map[string]any{
		"model":      pickTestModel(ch, "gpt-3.5-turbo"),
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
	}
	b, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("connect upstream: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upstream returned %d: %s", resp.StatusCode, snippet(body))
	}
	return nil
}

// testBedrock sends a minimal Converse request to the Bedrock runtime endpoint.
// Note: Bedrock auth uses a Bearer API key per Tech Design §6; the exact
// endpoint/auth is verified against AWS docs in T6. For a connectivity probe we
// hit the Converse path and treat any HTTP response as "reachable" only on 2xx.
func (s *ChannelService) testBedrock(ctx context.Context, ch *model.Channel, key string) error {
	if ch.Region == "" {
		return fmt.Errorf("region is required for bedrock channels")
	}
	modelID := pickTestModel(ch, "anthropic.claude-3-5-sonnet-20240620-v1:0")
	endpoint := fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com/model/%s/converse", ch.Region, modelID)
	reqBody := map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": []map[string]any{{"text": "ping"}}},
		},
		"inferenceConfig": map[string]any{"maxTokens": 1},
	}
	b, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("connect bedrock runtime: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("bedrock returned %d: %s", resp.StatusCode, snippet(body))
	}
	return nil
}

// testAnthropic sends a minimal Messages request to {base_url}/v1/messages to
// verify connectivity + credential. max_tokens is required by Anthropic, so a
// small value is always sent. Only a 2xx counts as success.
func (s *ChannelService) testAnthropic(ctx context.Context, ch *model.Channel, key string) error {
	base := strings.TrimRight(ch.BaseURL, "/")
	if base == "" {
		return fmt.Errorf("base_url is required for anthropic channels")
	}
	reqBody := map[string]any{
		"model":      pickTestModel(ch, "claude-opus-4-8"),
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
	}
	b, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/messages", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if key != "" {
		req.Header.Set("x-api-key", key)
	}

	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("connect upstream: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upstream returned %d: %s", resp.StatusCode, snippet(body))
	}
	return nil
}

// validChannelType reports whether t is a supported channel type.
func validChannelType(t model.ChannelType) bool {
	return t == model.ChannelOpenAI || t == model.ChannelBedrock || t == model.ChannelAnthropic
}

// toJSONArray marshals a string slice into a JSONB value (never nil).
func toJSONArray(items []string) datatypes.JSON {
	if items == nil {
		items = []string{}
	}
	b, err := json.Marshal(items)
	if err != nil {
		return datatypes.JSON([]byte("[]"))
	}
	return datatypes.JSON(b)
}

// snippet trims an upstream error body for inclusion in a readable message.
func snippet(b []byte) string {
	const max = 300
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	if s == "" {
		return "(empty body)"
	}
	return s
}
