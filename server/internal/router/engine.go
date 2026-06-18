// Package router implements the routing engine (Tech Design §5): it turns an
// inbound { group, model, estimatedPromptTokens } tuple into an ordered list of
// candidate upstream channels and drives load-balancing and failover.
//
// The four steps are:
//
//	(1) Match       — pick the first enabled rule (ascending priority) whose
//	                  every dimension is satisfied; no match falls back to all
//	                  enabled channels that can serve the model.
//	(2) Candidates  — resolve the rule's target_channel_ids / target_group to
//	                  enabled channels whose models include the target model
//	                  (model_mapping is considered).
//	(3) LoadBalance — bucket candidates by descending priority, take the highest
//	                  bucket, and pick one by weighted-random within it.
//	(4) Failover    — the remaining candidates (in load-balanced order) are
//	                  handed to the caller via Selection.Next so a failed attempt
//	                  can retry the next candidate, up to MaxRetries.
package router

import (
	"encoding/json"
	"errors"
	"math/rand"
	"sort"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"github.com/agent-router/server/internal/model"
)

// DefaultMaxRetries is the failover attempt cap when the option is unset.
const DefaultMaxRetries = 3

// OptMaxRetries is the option key controlling the failover retry cap.
const OptMaxRetries = "RouterMaxRetries"

// Engine-level sentinel errors. ErrNoCandidate is returned when no channel can
// serve the request at all; ErrFailoverExhausted is returned by Selection.Next
// once every candidate (within the retry budget) has been consumed.
var (
	ErrNoCandidate       = errors.New("no candidate channel can serve the request")
	ErrFailoverExhausted = errors.New("failover exhausted: all candidate channels failed")
)

// randIntn is indirected so tests can make weighted-random selection
// deterministic. It defaults to math/rand.Intn.
var randIntn = rand.Intn

// Engine selects upstream channels for relay requests. It reads routing rules
// and channels from the database on each selection so configuration changes
// take effect without a restart.
type Engine struct {
	db *gorm.DB
}

// NewEngine constructs an Engine backed by the given database handle.
func NewEngine(db *gorm.DB) *Engine {
	return &Engine{db: db}
}

// Selection is the result of SelectChannel: an ordered, load-balanced list of
// candidate channels plus the failover state used to walk it. The relay (T7)
// drives failover by calling Next repeatedly.
type Selection struct {
	// Rule is the matched routing rule, or nil when the no-rule fallback was used.
	Rule *model.RoutingRule
	// Model is the requested (external) model name.
	Model string

	// candidates is the full load-balanced order: highest-priority bucket first
	// (its members shuffled by weighted-random), then the remaining buckets in
	// descending priority order.
	candidates []model.Channel
	// maxRetries caps how many candidates Next will hand out (default 3).
	maxRetries int
	// cursor is the index of the next candidate to return.
	cursor int
}

// Candidates returns the full ordered candidate list (read-only view).
func (s *Selection) Candidates() []model.Channel {
	out := make([]model.Channel, len(s.candidates))
	copy(out, s.candidates)
	return out
}

// MaxRetries reports the failover attempt cap for this selection.
func (s *Selection) MaxRetries() int { return s.maxRetries }

// Next returns the next candidate channel to try, advancing the failover
// cursor. The first call yields the primary (load-balanced) choice; subsequent
// calls yield the failover candidates in order. It returns ErrFailoverExhausted
// once the candidates run out or the retry budget is reached.
func (s *Selection) Next() (*model.Channel, error) {
	if s.cursor >= len(s.candidates) || s.cursor >= s.maxRetries {
		return nil, ErrFailoverExhausted
	}
	ch := s.candidates[s.cursor]
	s.cursor++
	return &ch, nil
}

// UpstreamModel resolves the upstream model id for a channel, honouring its
// model_mapping (external name -> upstream id). When no mapping entry exists the
// external model name is used unchanged.
func UpstreamModel(ch *model.Channel, externalModel string) string {
	mapping := decodeMapping(ch.ModelMapping)
	if upstream, ok := mapping[externalModel]; ok && upstream != "" {
		return upstream
	}
	return externalModel
}

// SelectChannel runs the four-step pipeline and returns a Selection. group is
// the routing group from the caller's token, model is the requested model name
// and estTokens is the estimated prompt-token count used for the [min,max]
// dimension. It returns ErrNoCandidate when nothing can serve the request.
func (e *Engine) SelectChannel(group, requestedModel string, estTokens int) (*Selection, error) {
	// (1) Match.
	rules, err := e.enabledRulesByPriority()
	if err != nil {
		return nil, err
	}
	matched := matchRule(rules, group, requestedModel, estTokens)

	// (2) Candidates.
	candidates, err := e.candidateChannels(matched, requestedModel)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, ErrNoCandidate
	}

	// (3) LoadBalance — order the candidates: highest-priority bucket first
	// (weighted-random ordered), then remaining buckets descending.
	ordered := loadBalanceOrder(candidates)

	return &Selection{
		Rule:       matched,
		Model:      requestedModel,
		candidates: ordered,
		maxRetries: e.maxRetries(),
		cursor:     0,
	}, nil
}

// enabledRulesByPriority loads enabled rules in ascending-priority order.
func (e *Engine) enabledRulesByPriority() ([]model.RoutingRule, error) {
	var rules []model.RoutingRule
	err := e.db.Where("enabled = ?", true).
		Order("priority asc, id asc").
		Find(&rules).Error
	if err != nil {
		return nil, err
	}
	return rules, nil
}

// maxRetries reads the configurable failover cap, falling back to the default.
func (e *Engine) maxRetries() int {
	v := model.GetOption(e.db, OptMaxRetries, "")
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
		return n
	}
	return DefaultMaxRetries
}

// matchRule returns the first rule whose every dimension is satisfied, or nil
// when none match (no-rule fallback). Rules are assumed already sorted by
// ascending priority.
func matchRule(rules []model.RoutingRule, group, requestedModel string, estTokens int) *model.RoutingRule {
	for i := range rules {
		spec := decodeMatch(rules[i].Match)
		if ruleMatches(spec, group, requestedModel, estTokens) {
			return &rules[i]
		}
	}
	return nil
}

// ruleMatches reports whether all dimensions of spec are satisfied. An empty
// dimension means "unconstrained".
func ruleMatches(spec model.MatchSpec, group, requestedModel string, estTokens int) bool {
	// groups: must contain the group, or be empty.
	if len(spec.Groups) > 0 && !containsString(spec.Groups, group) {
		return false
	}
	// models: at least one pattern must match (wildcard '*' supported), or empty.
	if len(spec.Models) > 0 && !anyModelMatches(spec.Models, requestedModel) {
		return false
	}
	// min_tokens / max_tokens: estimated tokens must fall in [min, max]. A zero
	// bound means that side is unconstrained.
	if spec.MinTokens > 0 && estTokens < spec.MinTokens {
		return false
	}
	if spec.MaxTokens > 0 && estTokens > spec.MaxTokens {
		return false
	}
	return true
}

// candidateChannels resolves the candidate enabled channels for a matched rule.
// When rule is nil (no match) it falls back to every enabled channel that can
// serve the requested model. In all cases a candidate must be enabled and have
// the requested model in its model list (after model_mapping resolution).
func (e *Engine) candidateChannels(rule *model.RoutingRule, requestedModel string) ([]model.Channel, error) {
	var channels []model.Channel
	if err := e.db.Where("status = ?", model.ChannelEnabled).Find(&channels).Error; err != nil {
		return nil, err
	}

	// Build the rule's allowed-id / allowed-group filter (nil = no filter).
	var allowIDs map[uint]bool
	var allowGroup string
	if rule != nil {
		ids := decodeIDs(rule.TargetChannelIDs)
		if len(ids) > 0 {
			allowIDs = make(map[uint]bool, len(ids))
			for _, id := range ids {
				allowIDs[id] = true
			}
		}
		allowGroup = strings.TrimSpace(rule.TargetGroup)
	}

	out := make([]model.Channel, 0, len(channels))
	for i := range channels {
		ch := channels[i]
		if rule != nil {
			// A rule with explicit targets restricts to those; a rule with only
			// a target group restricts to that group. When both are empty the
			// rule targets all enabled channels.
			if allowIDs != nil {
				if !allowIDs[ch.ID] {
					continue
				}
			} else if allowGroup != "" && ch.Group != allowGroup {
				continue
			}
		}
		if !channelServes(&ch, requestedModel) {
			continue
		}
		out = append(out, ch)
	}
	return out, nil
}

// channelServes reports whether the channel's model list includes the requested
// model. model_mapping keys (external names) also count as served models.
func channelServes(ch *model.Channel, requestedModel string) bool {
	for _, m := range decodeModels(ch.Models) {
		if m == requestedModel {
			return true
		}
	}
	// A model_mapping entry keyed by the external model name means the channel
	// serves that external model (mapping it to a different upstream id).
	if _, ok := decodeMapping(ch.ModelMapping)[requestedModel]; ok {
		return true
	}
	return false
}

// loadBalanceOrder orders candidates for failover: it groups them into priority
// buckets (higher priority preferred), and within the highest bucket performs a
// weighted-random shuffle by channel weight. Lower buckets follow in descending
// priority order (each also weighted-shuffled) so failover degrades gracefully.
func loadBalanceOrder(candidates []model.Channel) []model.Channel {
	if len(candidates) <= 1 {
		return candidates
	}

	// Distinct priorities, descending.
	bucketsByPrio := map[int][]model.Channel{}
	for _, ch := range candidates {
		bucketsByPrio[ch.Priority] = append(bucketsByPrio[ch.Priority], ch)
	}
	prios := make([]int, 0, len(bucketsByPrio))
	for p := range bucketsByPrio {
		prios = append(prios, p)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(prios)))

	ordered := make([]model.Channel, 0, len(candidates))
	for _, p := range prios {
		ordered = append(ordered, weightedShuffle(bucketsByPrio[p])...)
	}
	return ordered
}

// weightedShuffle returns the bucket reordered by repeated weighted-random
// selection so the first element is the weighted-random primary pick. Channels
// with non-positive weight are treated as weight 1.
func weightedShuffle(bucket []model.Channel) []model.Channel {
	pool := make([]model.Channel, len(bucket))
	copy(pool, bucket)

	out := make([]model.Channel, 0, len(pool))
	for len(pool) > 0 {
		total := 0
		for i := range pool {
			total += effectiveWeight(pool[i].Weight)
		}
		pick := randIntn(total)
		idx := 0
		for i := range pool {
			pick -= effectiveWeight(pool[i].Weight)
			if pick < 0 {
				idx = i
				break
			}
		}
		out = append(out, pool[idx])
		pool = append(pool[:idx], pool[idx+1:]...)
	}
	return out
}

func effectiveWeight(w int) int {
	if w <= 0 {
		return 1
	}
	return w
}

// --- JSONB decode helpers -------------------------------------------------

func decodeMatch(raw []byte) model.MatchSpec {
	var spec model.MatchSpec
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &spec)
	}
	return spec
}

func decodeIDs(raw []byte) []uint {
	var ids []uint
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &ids)
	}
	return ids
}

func decodeModels(raw []byte) []string {
	var models []string
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &models)
	}
	return models
}

func decodeMapping(raw []byte) map[string]string {
	mapping := map[string]string{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &mapping)
	}
	return mapping
}

// --- small string helpers -------------------------------------------------

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// anyModelMatches reports whether the requested model matches any pattern.
func anyModelMatches(patterns []string, requestedModel string) bool {
	for _, p := range patterns {
		if modelMatches(p, requestedModel) {
			return true
		}
	}
	return false
}

// modelMatches reports whether pattern matches model. A bare "*" matches
// anything; a pattern may contain "*" wildcards each matching any run of
// characters (e.g. "claude-*", "*-mini", "gpt-*-turbo").
func modelMatches(pattern, requestedModel string) bool {
	if pattern == requestedModel {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return false
	}
	return wildcardMatch(pattern, requestedModel)
}

// wildcardMatch matches s against a glob with '*' wildcards (no other
// metacharacters). It splits the pattern on '*' and greedily matches the
// literal segments in order.
func wildcardMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	// Leading segment must be a prefix.
	if first := parts[0]; first != "" {
		if !strings.HasPrefix(s, first) {
			return false
		}
		s = s[len(first):]
	}
	// Trailing segment must be a suffix.
	last := parts[len(parts)-1]
	if last != "" {
		if !strings.HasSuffix(s, last) {
			return false
		}
		s = s[:len(s)-len(last)]
	}
	// Middle segments must appear in order.
	for _, mid := range parts[1 : len(parts)-1] {
		if mid == "" {
			continue
		}
		idx := strings.Index(s, mid)
		if idx < 0 {
			return false
		}
		s = s[idx+len(mid):]
	}
	return true
}
