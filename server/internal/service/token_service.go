package service

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/model"
)

// Token-related sentinel errors. ErrTokenForbidden is returned when a user
// attempts to access a token that belongs to another user, so the controller
// can map it to 403/404 (ownership gating, Tech Design §8).
var (
	ErrTokenNotFound  = errors.New("token not found")
	ErrInvalidToken   = errors.New("invalid token input")
	ErrTokenForbidden = errors.New("token does not belong to user")
)

// tokenKeyPrefix is the human-visible prefix of every generated downstream key
// (Tech Design §3 tokens). The plaintext is shown to the user only on creation.
const tokenKeyPrefix = "sk-"

// tokenRandomBytes is the number of random bytes used for the key body. 32 bytes
// (256 bits) hex-encoded yields a 64-char body, well beyond guessing.
const tokenRandomBytes = 32

// TokenService implements downstream API-key (sk-...) management: generation,
// CRUD and ownership-scoped lookups (Tech Design §3 tokens, §8). The plaintext
// key is never persisted; only its sha256 hash (key_hash, a unique index used
// for relay lookup) and a display mask are stored/returned.
type TokenService struct {
	db *gorm.DB
}

// NewTokenService constructs a TokenService.
func NewTokenService(db *gorm.DB) *TokenService {
	return &TokenService{db: db}
}

// TokenView is the outward representation of a token. It embeds the stored model
// (with the plaintext Key column cleared) and adds a non-reversible display
// mask. The full plaintext key is returned exactly once, on creation, via the
// separate PlainKey field of CreateTokenResult.
type TokenView struct {
	model.Token
	KeyMasked string `json:"key_masked"`
}

// CreateTokenResult is returned by Create. PlainKey carries the full sk- key and
// is populated only here — it is the single opportunity for the caller to read
// the plaintext (Tech Design §3: 明文仅创建时返回一次).
type CreateTokenResult struct {
	TokenView
	PlainKey string `json:"key"`
}

// TokenInput carries the writable fields of a token. Pointer fields on update
// mean "leave unchanged when nil"; on create, nil falls back to defaults.
type TokenInput struct {
	Name          *string
	Status        *model.TokenStatus
	Quota         *int64
	ExpiredAt     *time.Time
	Group         *string
	AllowedModels *[]string
	// OutputFormat pins the response rendering format (openai|anthropic|bedrock);
	// a non-nil empty string clears it (= follow the endpoint). nil = unchanged.
	OutputFormat *string
}

// HashKey returns the sha256 hex digest of a plaintext key. The relay auth
// middleware (T7) hashes the inbound sk- key and looks the token up by this
// value, so the function is exported for reuse.
func HashKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// GenerateKey produces a new random downstream key of the form "sk-<hex>".
func GenerateKey() (string, error) {
	buf := make([]byte, tokenRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}
	return tokenKeyPrefix + hex.EncodeToString(buf), nil
}

// toView builds a masked view of a stored token. The plaintext Key column is
// always empty (never persisted), so the mask is derived from the stored prefix
// and the key_hash suffix to give a stable, recognisable display value.
func (s *TokenService) toView(t *model.Token) TokenView {
	v := TokenView{Token: *t}
	v.Token.Key = nil // never leak any plaintext via the embedded model
	v.KeyMasked = maskFromHash(t.KeyHash)
	return v
}

// maskFromHash returns a stable display mask "sk-...<last4-of-hash>" for a token
// whose plaintext is no longer available. It reveals only the constant prefix
// and a few trailing hash characters (non-reversible).
func maskFromHash(keyHash string) string {
	if keyHash == "" {
		return ""
	}
	tail := keyHash
	if len(tail) > 6 {
		tail = tail[len(tail)-6:]
	}
	return tokenKeyPrefix + "****" + tail
}

// Create generates a new sk- key for the given user and persists only its hash
// (plus metadata). The returned result carries the plaintext key exactly once.
func (s *TokenService) Create(userID uint, in TokenInput) (*CreateTokenResult, error) {
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidToken)
	}

	plain, err := GenerateKey()
	if err != nil {
		return nil, err
	}

	// Resolve the group with this precedence (Tech Design §2.6):
	//  1. an explicit, non-empty group on the input wins;
	//  2. otherwise (absent or empty) inherit the owning user's Group;
	//  3. hard fallback to "default" if the user lookup fails / yields empty,
	//     so a token is never created with an empty group.
	// This is resolved here and assigned AFTER applyInput so applyInput's own
	// empty->"default" handling (shared with Update) cannot clobber an inherited
	// group when the input carried an explicit empty string. Engine routing
	// semantics are unchanged — it still keys off the resolved token.group.
	group := "default"
	if in.Group != nil && strings.TrimSpace(*in.Group) != "" {
		group = strings.TrimSpace(*in.Group)
	} else if g := s.userGroup(userID); g != "" {
		group = g
	}

	t := &model.Token{
		UserID:    userID,
		Name:      strings.TrimSpace(*in.Name),
		KeyHash:   HashKey(plain),
		Status:    model.TokenEnabled,
		Quota:     model.QuotaUnlimited,
		UsedQuota: 0,
	}
	s.applyInput(t, in)
	// Authoritative group resolution (after applyInput, which may have set it to
	// "default" from an explicit empty string).
	t.Group = group

	if t.AllowedModels == nil {
		t.AllowedModels = datatypes.JSON([]byte("[]"))
	}

	if err := s.db.Create(t).Error; err != nil {
		return nil, err
	}

	res := &CreateTokenResult{TokenView: s.toView(t), PlainKey: plain}
	return res, nil
}

// userGroup returns the owning user's default routing group, or "" if it cannot
// be read (missing user / DB error). Used to seed a new token's group when the
// create request omits one (Tech Design §2.6).
func (s *TokenService) userGroup(userID uint) string {
	// Select the full row rather than just the "group" column: group is a
	// reserved word in SQL and selecting it bare would need dialect-specific
	// quoting. GORM quotes column identifiers correctly when mapping the struct.
	var u model.User
	if err := s.db.First(&u, userID).Error; err != nil {
		return ""
	}
	return strings.TrimSpace(u.Group)
}

// Update applies a partial update to a token the user owns. Only non-nil input
// fields change. The key itself is immutable (regeneration = create a new one).
func (s *TokenService) Update(userID, id uint, in TokenInput) (*TokenView, error) {
	t, err := s.getOwned(userID, id)
	if err != nil {
		return nil, err
	}
	if in.Name != nil && strings.TrimSpace(*in.Name) == "" {
		return nil, fmt.Errorf("%w: name cannot be empty", ErrInvalidToken)
	}

	s.applyInput(t, in)

	if err := s.db.Save(t).Error; err != nil {
		return nil, err
	}
	v := s.toView(t)
	return &v, nil
}

// applyInput copies non-nil writable fields from in onto t.
func (s *TokenService) applyInput(t *model.Token, in TokenInput) {
	if in.Name != nil {
		t.Name = strings.TrimSpace(*in.Name)
	}
	if in.Status != nil {
		t.Status = *in.Status
	}
	if in.Quota != nil {
		t.Quota = *in.Quota
	}
	if in.ExpiredAt != nil {
		// Treat the zero time as "clear expiry".
		if in.ExpiredAt.IsZero() {
			t.ExpiredAt = nil
		} else {
			ea := *in.ExpiredAt
			t.ExpiredAt = &ea
		}
	}
	if in.Group != nil {
		g := strings.TrimSpace(*in.Group)
		if g == "" {
			g = "default"
		}
		t.Group = g
	}
	if in.AllowedModels != nil {
		t.AllowedModels = toJSONArray(*in.AllowedModels)
	}
	if in.OutputFormat != nil {
		// Empty string clears the override (follow endpoint); else store the value.
		v := strings.TrimSpace(*in.OutputFormat)
		if v == "" {
			t.OutputFormat = nil
		} else {
			t.OutputFormat = &v
		}
	}
}

// Delete removes a token the user owns. Deleting a non-existent or
// other-user token yields ErrTokenNotFound (ownership gating).
func (s *TokenService) Delete(userID, id uint) error {
	res := s.db.Where("id = ? AND user_id = ?", id, userID).Delete(&model.Token{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrTokenNotFound
	}
	return nil
}

// Get returns a single token (masked) the user owns.
func (s *TokenService) Get(userID, id uint) (*TokenView, error) {
	t, err := s.getOwned(userID, id)
	if err != nil {
		return nil, err
	}
	v := s.toView(t)
	return &v, nil
}

// List returns all tokens (masked) owned by the user, ordered by id.
func (s *TokenService) List(userID uint) ([]TokenView, error) {
	var ts []model.Token
	if err := s.db.Where("user_id = ?", userID).Order("id asc").Find(&ts).Error; err != nil {
		return nil, err
	}
	out := make([]TokenView, 0, len(ts))
	for i := range ts {
		out = append(out, s.toView(&ts[i]))
	}
	return out, nil
}

// DistinctGroups returns the set of routing groups in use across all tokens AND
// channels, sorted. Routing rules match on the token's group (match.groups) and
// can target a channel group, so both populate the rule editor's group dropdown.
// Admin-scoped (not per-user): a routing rule is a global, admin-only construct.
func (s *TokenService) DistinctGroups() ([]string, error) {
	set := map[string]bool{}
	var tokenGroups []string
	if err := s.db.Model(&model.Token{}).Distinct().Pluck("group", &tokenGroups).Error; err != nil {
		return nil, err
	}
	for _, g := range tokenGroups {
		if g = strings.TrimSpace(g); g != "" {
			set[g] = true
		}
	}
	var chanGroups []string
	if err := s.db.Model(&model.Channel{}).Distinct().Pluck("\"group\"", &chanGroups).Error; err != nil {
		return nil, err
	}
	for _, g := range chanGroups {
		if g = strings.TrimSpace(g); g != "" {
			set[g] = true
		}
	}
	out := make([]string, 0, len(set))
	for g := range set {
		out = append(out, g)
	}
	sort.Strings(out)
	return out, nil
}

// GetByKeyHash loads a token by its key_hash. The relay auth middleware (T7)
// uses this to resolve an inbound sk- key to its token. It returns the raw model
// (not a view) because the relay needs the live quota fields.
func (s *TokenService) GetByKeyHash(keyHash string) (*model.Token, error) {
	var t model.Token
	err := s.db.Where("key_hash = ?", keyHash).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrTokenNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// getOwned loads a token by id and verifies it belongs to userID. A token owned
// by another user is reported as ErrTokenNotFound so cross-user probing cannot
// distinguish "exists but not yours" from "does not exist" (ownership gating).
func (s *TokenService) getOwned(userID, id uint) (*model.Token, error) {
	var t model.Token
	err := s.db.First(&t, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrTokenNotFound
	}
	if err != nil {
		return nil, err
	}
	if t.UserID != userID {
		return nil, ErrTokenNotFound
	}
	return &t, nil
}
