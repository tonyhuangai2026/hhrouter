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
//	                  enabled channels whose models include the EFFECTIVE model
//	                  (model_mapping is considered) — the rule's target_model when
//	                  it overrides one, else the requested model.
//	(3) LoadBalance — bucket candidates by descending priority, take the highest
//	                  bucket, and pick one by weighted-random within it.
//	(4) Failover    — the remaining candidates (in load-balanced order) are
//	                  handed to the caller via Selection.Next so a failed attempt
//	                  can retry the next candidate, up to MaxRetries.
package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"github.com/agent-router/server/internal/model"
	"github.com/agent-router/server/internal/router/expr"
	"github.com/agent-router/server/internal/router/probe"
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
	// probeResolver returns the routing classifier to use for the CURRENT request,
	// resolved fresh each time so a settings change (mock⇄real, URL edit) takes
	// effect without a restart. It returns nil when no probe is configured. The
	// probe is invoked ON DEMAND — only when an enabled rule's expression actually
	// references w or t — so probe-less routing incurs zero extra latency.
	probeResolver func() probe.Probe
}

// NewEngine constructs an Engine backed by the given database handle.
func NewEngine(db *gorm.DB) *Engine {
	return &Engine{db: db}
}

// WithProbe sets a FIXED routing classifier (resolved the same every request).
// Used by tests; production uses WithProbeResolver to honour runtime settings.
func (e *Engine) WithProbe(p probe.Probe) *Engine {
	e.probeResolver = func() probe.Probe { return p }
	return e
}

// WithProbeResolver sets a per-request probe resolver (chainable). Returning nil
// means "no probe this request".
func (e *Engine) WithProbeResolver(fn func() probe.Probe) *Engine {
	e.probeResolver = fn
	return e
}

// currentProbe resolves the probe for this request (nil-safe).
func (e *Engine) currentProbe() probe.Probe {
	if e.probeResolver == nil {
		return nil
	}
	return e.probeResolver()
}

// RouteInput carries everything SelectChannelCtx needs, including the rendered
// conversation prompt the probe classifies. Prompt may be empty (the probe is
// then only invoked if a rule references w/t, and classifies the empty string).
type RouteInput struct {
	Group     string
	Model     string
	EstTokens int
	// Prompt is the conversation context rendered to the classifier's expected
	// single-string form (see RenderProbePrompt).
	Prompt string
}

// ProbeResult records what the routing classifier ("small model" probe)
// predicted for a request, so the relay can log it and the UI can show "this
// request was routed with w=.., t=..". It is set on the Selection only when the
// probe was actually invoked (an enabled rule referenced w/t).
type ProbeResult struct {
	W    int    `json:"w"`
	T    int    `json:"t"`
	Name string `json:"name"`          // probe implementation ("mock" / "http")
	Err  string `json:"err,omitempty"` // set when the probe call failed (w/t then 0)
}

// Selection is the result of SelectChannel: an ordered, load-balanced list of
// candidate channels plus the failover state used to walk it. The relay (T7)
// drives failover by calling Next repeatedly.
type Selection struct {
	// Rule is the matched routing rule, or nil when the no-rule fallback was used.
	Rule *model.RoutingRule
	// Model is the requested (external) model name. It stays the CLIENT's name even
	// when the matched rule overrides the model — observability distinguishes "what
	// was asked for" from "what actually ran" (see EffectiveModel).
	Model string
	// Probe is the routing-classifier prediction for this request, or nil when the
	// probe was not invoked (no enabled rule referenced w/t).
	Probe *ProbeResult

	// effectiveModel is the external model name that actually drove candidate
	// filtering and must drive upstream model resolution: the matched rule's
	// TargetModel when it is set, otherwise Model. Read via EffectiveModel.
	effectiveModel string
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

// EffectiveModel returns the external model name that actually serves this
// request: the matched rule's target_model when it set one, otherwise the
// client's requested model. Callers resolving the upstream model id must feed
// THIS (not Model) to UpstreamModel, so the channel's model_mapping is applied
// on top of the override. It never returns empty — a Selection built without an
// override falls back to Model.
func (s *Selection) EffectiveModel() string {
	if s.effectiveModel != "" {
		return s.effectiveModel
	}
	return s.Model
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

// Probe prompt size limits (characters). The classifier was trained with an
// 8192-token cap and prompts up to ~20.7k chars; a request far larger than that
// is both out-of-distribution AND gets rejected before the model even sees it
// (nginx 413 in front of the endpoint, then vLLM's max-model-len). We therefore
// bound the rendered prompt on the CLIENT side:
//   - maxProbePromptChars caps the whole prompt, keeping the most RECENT turns
//     (the write/tool signal lives in the last tool_use/tool_result, not deep
//     history), while always retaining the trailing open assistant turn.
//   - maxProbeTurnChars caps any single oversized turn (e.g. a huge tool_result
//     dump); the middle is elided with an "…[truncated]" marker, matching the
//     "[truncated]" convention present in the training data.
const (
	maxProbePromptChars = 16000
	maxProbeTurnChars   = 4000
)

// RenderProbePrompt renders a conversation (role/text pairs, oldest first) into
// the single-string form the routing classifier expects — the Qwen chat-template
// layout from the API reference: each turn wrapped as
// "<|im_start|>{role}\n{text}<|im_end|>\n", ending with an open assistant turn
// "<|im_start|>assistant\n" so the classifier predicts the NEXT assistant turn.
// system text (if any) is prepended as a leading system turn.
//
// The result is size-bounded (see maxProbePromptChars): oversized single turns
// are truncated in the middle, and if the whole conversation still exceeds the
// budget the OLDEST turns are dropped first so the most recent context — where
// the routing signal is — is preserved.
func RenderProbePrompt(system string, turns []struct{ Role, Text string }) string {
	const suffix = "<|im_start|>assistant\n"

	// Render one turn, truncating its content if it alone is too large.
	renderTurn := func(role, text string) string {
		return "<|im_start|>" + role + "\n" + truncateMiddle(text, maxProbeTurnChars) + "<|im_end|>\n"
	}

	var head string
	if strings.TrimSpace(system) != "" {
		head = "<|im_start|>system\n" + truncateMiddle(system, maxProbeTurnChars) + "<|im_end|>\n"
	}

	// Budget left for turns after reserving the trailing assistant marker. The
	// system turn competes for the same budget but is lower priority than recent
	// turns, so we add turns newest-first and only then prepend the system head if
	// it still fits.
	budget := maxProbePromptChars - len(suffix)

	// Walk turns newest → oldest, prepending each while it fits.
	rendered := make([]string, 0, len(turns))
	used := 0
	for i := len(turns) - 1; i >= 0; i-- {
		seg := renderTurn(turns[i].Role, turns[i].Text)
		if used+len(seg) > budget && len(rendered) > 0 {
			break // keep what we have; drop older turns
		}
		rendered = append(rendered, seg)
		used += len(seg)
	}
	// rendered is newest-first; reverse to chronological order.
	var b strings.Builder
	if head != "" && used+len(head) <= budget {
		b.WriteString(head)
	}
	for i := len(rendered) - 1; i >= 0; i-- {
		b.WriteString(rendered[i])
	}
	b.WriteString(suffix)
	return b.String()
}

// truncateMiddle returns s unchanged when it fits in max characters; otherwise it
// keeps the head and tail and elides the middle with an "…[truncated]…" marker.
// Both ends are retained because a tool_result's shape (start) and its outcome
// (end) can both carry signal. max is a character budget (not bytes); it operates
// on runes so multibyte text is never split.
func truncateMiddle(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	const marker = "…[truncated]…"
	keep := max - len([]rune(marker))
	if keep < 2 {
		return string(r[:max])
	}
	headN := keep / 2
	tailN := keep - headN
	return string(r[:headN]) + marker + string(r[len(r)-tailN:])
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

// SelectChannel runs the pipeline with no probe context (no custom-expression
// w/t signals). Retained for callers/tests that don't supply a prompt; rules
// whose expressions reference w/t will see w=0,t=0. Prefer SelectChannelCtx.
func (e *Engine) SelectChannel(group, requestedModel string, estTokens int) (*Selection, error) {
	return e.SelectChannelCtx(context.Background(), RouteInput{
		Group: group, Model: requestedModel, EstTokens: estTokens,
	})
}

// SelectChannelCtx runs the four-step pipeline and returns a Selection. It adds
// custom-expression matching: each enabled rule may carry an Expr evaluated on
// top of its Match predicate. If any enabled rule's expression references the
// probe signals (w/t), the probe is invoked ONCE (on demand) and the result is
// fed into every expression's evaluation. Rules with no w/t reference never
// trigger the probe.
//
// The matched rule's target_model (when set) becomes the selection's effective
// model and drives candidate filtering; see Selection.EffectiveModel. Returns an
// error wrapping ErrNoCandidate when nothing can serve the request.
func (e *Engine) SelectChannelCtx(ctx context.Context, in RouteInput) (*Selection, error) {
	// (1) Match.
	rules, err := e.enabledRulesByPriority()
	if err != nil {
		return nil, err
	}

	// Compile each rule's expression once; collect whether any needs the probe.
	progs := make([]*expr.Program, len(rules))
	needProbe := false
	for i := range rules {
		p, cerr := expr.Compile(rules[i].Expr)
		if cerr != nil {
			// A stored expression failed to compile (shouldn't happen — validated on
			// save). Treat as an always-false condition so the rule is skipped rather
			// than crashing routing.
			progs[i] = nil
			continue
		}
		progs[i] = p
		if p.References(expr.VarW) || p.References(expr.VarT) {
			needProbe = true
		}
	}

	// Invoke the probe on demand. A probe failure is non-fatal: w/t default to 0
	// (expressions referencing them still evaluate, just with zero signals).
	exprVars := expr.Vars{
		Int: map[string]int{expr.VarTokens: in.EstTokens},
		Str: map[string]string{expr.VarGroup: in.Group, expr.VarModel: in.Model},
	}
	var probeResult *ProbeResult
	if needProbe {
		if p := e.currentProbe(); p != nil {
			pr := &ProbeResult{Name: p.Name()}
			if pred, perr := p.Predict(ctx, in.Prompt); perr == nil {
				pr.W, pr.T = pred.W, pred.T
				exprVars.Int[expr.VarW] = pred.W
				exprVars.Int[expr.VarT] = pred.T
			} else {
				pr.Err = perr.Error()
			}
			probeResult = pr
		}
	}

	matched, matchedProg := matchRuleExpr(rules, progs, in.Group, in.Model, in.EstTokens, exprVars)
	_ = matchedProg

	// The matched rule may override which model serves the request. The override
	// takes effect from here on: it filters candidates (a channel need only serve
	// the TARGET model, not the requested one) and later resolves the upstream
	// model id. With no override — and always on the no-rule fallback path — the
	// effective model is the requested model, byte-for-byte the legacy behaviour.
	effectiveModel := in.Model
	if matched != nil {
		if target := strings.TrimSpace(matched.TargetModel); target != "" {
			effectiveModel = target
		}
	}

	// (2) Candidates.
	candidates, err := e.candidateChannels(matched, effectiveModel)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		// Deliberately NO rule backtracking: the matched rule is the final routing
		// decision, so a target_model nothing can serve is a hard failure rather
		// than a silent downgrade to a lower-priority rule (which would defeat the
		// whole point of quality-tiered routing). Name the rule and the target model
		// so operators can tell a misconfigured override apart from a plain
		// "nobody serves the requested model", and wrap the sentinel so
		// errors.Is(err, ErrNoCandidate) keeps working for existing callers.
		// Name the rule whenever one matched: with channel routing taking
		// precedence, an empty candidate set under a rule means its target
		// channels are all gone/disabled (the model name no longer filters them),
		// and the operator needs to know WHICH rule pointed nowhere.
		if matched != nil {
			if target := strings.TrimSpace(matched.TargetModel); target != "" {
				return nil, fmt.Errorf("%w: rule %s has no enabled target channel (target_model %q, request asked for %q)",
					ErrNoCandidate, ruleIdent(matched), target, in.Model)
			}
			return nil, fmt.Errorf("%w: rule %s has no enabled target channel able to serve %q",
				ErrNoCandidate, ruleIdent(matched), in.Model)
		}
		return nil, ErrNoCandidate
	}

	// (3) LoadBalance.
	ordered := loadBalanceOrder(candidates)

	return &Selection{
		Rule:           matched,
		Model:          in.Model,
		Probe:          probeResult,
		effectiveModel: effectiveModel,
		candidates:     ordered,
		maxRetries:     e.maxRetries(),
		cursor:         0,
	}, nil
}

// ruleIdent renders a human-readable identifier for a rule ("name" plus its id)
// for error messages and logs. It is nil-safe.
func ruleIdent(rule *model.RoutingRule) string {
	if rule == nil {
		return "<none>"
	}
	return fmt.Sprintf("%q (id %d)", rule.Name, rule.ID)
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
// ascending priority. (No expression support — used by tests and the probe-less
// path via matchRuleExpr with nil programs.)
func matchRule(rules []model.RoutingRule, group, requestedModel string, estTokens int) *model.RoutingRule {
	r, _ := matchRuleExpr(rules, make([]*expr.Program, len(rules)), group, requestedModel, estTokens, expr.Vars{})
	return r
}

// matchRuleExpr returns the first rule whose Match predicate AND custom
// expression both pass. progs[i] is the compiled expression for rules[i] (nil =
// treat the expression as failing, so a rule with a broken stored expression is
// skipped rather than matched). A nil program for a rule with an EMPTY Expr is
// fine because expr.Compile("") yields an always-true program — callers pass
// real compiled programs; the all-nil slice from matchRule means "no rule has an
// expression", and an empty-Expr rule compiled to a non-nil always-true program.
func matchRuleExpr(rules []model.RoutingRule, progs []*expr.Program, group, requestedModel string, estTokens int, vars expr.Vars) (*model.RoutingRule, *expr.Program) {
	for i := range rules {
		spec := decodeMatch(rules[i].Match)
		if !ruleMatches(spec, group, requestedModel, estTokens) {
			continue
		}
		// Expression gate: only applies when the rule actually has one.
		if strings.TrimSpace(rules[i].Expr) != "" {
			p := progs[i]
			if p == nil || !p.Eval(vars) {
				continue
			}
		}
		return &rules[i], progs[i]
	}
	return nil, nil
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
//
// Channel routing takes precedence over model routing: when the rule names its
// targets explicitly (target_channel_ids or target_group) those targets ARE the
// decision, and the model name is not used to filter them further. Inside a
// chosen channel the model is then settled separately by target_model (see
// SelectChannelCtx) — so a rule may legitimately send a request for "opus" to a
// channel that only lists cheaper models, which is the whole point of tiering
// within one channel.
//
// The model-name filter still applies when the rule does NOT name targets, and
// on the no-rule fallback path. There the requested model is the only signal
// available, and without it the request could land on a channel that cannot
// serve it at all.
//
// effectiveModel is the rule's target_model when it overrides one, otherwise the
// client's requested model.
func (e *Engine) candidateChannels(rule *model.RoutingRule, effectiveModel string) ([]model.Channel, error) {
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

	// Did the rule actually pin its targets? If so the model name must not narrow
	// them any further — that second gate is what used to drop a rule's own
	// channel merely because the channel had not declared whatever name the
	// client happened to send.
	targetsPinned := allowIDs != nil || allowGroup != ""

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
		if !targetsPinned && !channelServes(&ch, effectiveModel) {
			continue
		}
		out = append(out, ch)
	}
	return out, nil
}

// channelServes reports whether the channel's model list includes wantModel (an
// external model name). model_mapping keys (external names) also count as served
// models.
func channelServes(ch *model.Channel, wantModel string) bool {
	for _, m := range decodeModels(ch.Models) {
		if m == wantModel {
			return true
		}
	}
	// A model_mapping entry keyed by the external model name means the channel
	// serves that external model (mapping it to a different upstream id).
	if _, ok := decodeMapping(ch.ModelMapping)[wantModel]; ok {
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
