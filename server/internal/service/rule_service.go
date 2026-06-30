package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/model"
	"github.com/agent-router/server/internal/router/expr"
)

// Rule-related sentinel errors.
var (
	ErrRuleNotFound = errors.New("routing rule not found")
	ErrInvalidRule  = errors.New("invalid routing rule input")
)

// RuleService implements routing-rule CRUD (Tech Design §3 routing_rules, §8).
// A rule carries a name, an enabled flag, an ascending-priority key, a JSONB
// match predicate (groups/models/min_tokens/max_tokens) and a target channel
// set expressed either as explicit channel ids or a channel group.
type RuleService struct {
	db *gorm.DB
}

// NewRuleService constructs a RuleService.
func NewRuleService(db *gorm.DB) *RuleService {
	return &RuleService{db: db}
}

// RuleInput carries the writable fields of a routing rule. Pointer fields on
// update mean "leave unchanged when nil"; on create, nil falls back to defaults.
type RuleInput struct {
	Name             *string
	Enabled          *bool
	Priority         *int
	Match            *model.MatchSpec
	TargetChannelIDs *[]uint
	TargetGroup      *string
	Expr             *string
}

// Create validates and persists a new routing rule.
func (s *RuleService) Create(in RuleInput) (*model.RoutingRule, error) {
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidRule)
	}

	rule := &model.RoutingRule{
		Enabled:  true,
		Priority: 0,
	}
	if err := s.applyInput(rule, in); err != nil {
		return nil, err
	}

	// Ensure JSONB columns are never NULL so reads decode cleanly.
	if rule.Match == nil {
		rule.Match = datatypes.JSON([]byte("{}"))
	}
	if rule.TargetChannelIDs == nil {
		rule.TargetChannelIDs = datatypes.JSON([]byte("[]"))
	}

	wantEnabled := rule.Enabled
	if err := s.db.Create(rule).Error; err != nil {
		return nil, err
	}
	// The model's `enabled default:true` tag makes GORM coerce a zero-value
	// (false) Enabled to true on insert; force the intended value back when the
	// caller explicitly requested a disabled rule.
	if !wantEnabled && rule.Enabled {
		if err := s.db.Model(rule).Update("enabled", false).Error; err != nil {
			return nil, err
		}
		rule.Enabled = false
	}
	return rule, nil
}

// Update applies a partial update to an existing rule.
func (s *RuleService) Update(id uint, in RuleInput) (*model.RoutingRule, error) {
	rule, err := s.getRaw(id)
	if err != nil {
		return nil, err
	}
	if in.Name != nil && strings.TrimSpace(*in.Name) == "" {
		return nil, fmt.Errorf("%w: name cannot be empty", ErrInvalidRule)
	}
	if err := s.applyInput(rule, in); err != nil {
		return nil, err
	}
	if err := s.db.Save(rule).Error; err != nil {
		return nil, err
	}
	return rule, nil
}

// applyInput copies non-nil fields from in onto rule, marshalling the structured
// match/target fields into their JSONB columns.
func (s *RuleService) applyInput(rule *model.RoutingRule, in RuleInput) error {
	if in.Name != nil {
		rule.Name = strings.TrimSpace(*in.Name)
	}
	if in.Enabled != nil {
		rule.Enabled = *in.Enabled
	}
	if in.Priority != nil {
		rule.Priority = *in.Priority
	}
	if in.Match != nil {
		b, err := json.Marshal(*in.Match)
		if err != nil {
			return fmt.Errorf("%w: match: %v", ErrInvalidRule, err)
		}
		rule.Match = datatypes.JSON(b)
	}
	if in.TargetChannelIDs != nil {
		ids := *in.TargetChannelIDs
		if ids == nil {
			ids = []uint{}
		}
		b, err := json.Marshal(ids)
		if err != nil {
			return fmt.Errorf("%w: target_channel_ids: %v", ErrInvalidRule, err)
		}
		rule.TargetChannelIDs = datatypes.JSON(b)
	}
	if in.TargetGroup != nil {
		rule.TargetGroup = strings.TrimSpace(*in.TargetGroup)
	}
	if in.Expr != nil {
		e := strings.TrimSpace(*in.Expr)
		// Validate by compiling: a bad expression is a 400-class input error so the
		// rule editor can surface the parser message.
		if _, err := expr.Compile(e); err != nil {
			return fmt.Errorf("%w: expr: %v", ErrInvalidRule, err)
		}
		rule.Expr = e
	}
	return nil
}

// Delete removes a rule by id.
func (s *RuleService) Delete(id uint) error {
	res := s.db.Delete(&model.RoutingRule{}, id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrRuleNotFound
	}
	return nil
}

// Get returns a single routing rule by id.
func (s *RuleService) Get(id uint) (*model.RoutingRule, error) {
	return s.getRaw(id)
}

// List returns all routing rules ordered by ascending priority then id (the
// same order the engine evaluates them in).
func (s *RuleService) List() ([]model.RoutingRule, error) {
	var rules []model.RoutingRule
	if err := s.db.Order("priority asc, id asc").Find(&rules).Error; err != nil {
		return nil, err
	}
	return rules, nil
}

// getRaw loads a rule, mapping a missing row onto ErrRuleNotFound.
func (s *RuleService) getRaw(id uint) (*model.RoutingRule, error) {
	var rule model.RoutingRule
	err := s.db.First(&rule, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrRuleNotFound
	}
	if err != nil {
		return nil, err
	}
	return &rule, nil
}
