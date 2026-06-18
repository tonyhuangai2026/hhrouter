package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/agent-router/server/internal/model"
)

// Quota-related sentinel errors.
var (
	ErrQuotaTokenNotFound = errors.New("quota: token not found")
	ErrQuotaUserNotFound  = errors.New("quota: user not found")
)

// QuotaUnlimited mirrors model.QuotaUnlimited for callers importing only this
// package; quota == -1 means "no limit" at either level (Tech Design §4).
const QuotaUnlimited = model.QuotaUnlimited

// Redis key prefixes for the real-time used-quota counters. Each holds the
// number of tokens consumed so far; remaining is derived as quota - counter.
const (
	tokenUsedKeyPrefix = "quota:token:used:"
	userUsedKeyPrefix  = "quota:user:used:"
)

// counterTTL bounds how long a backfilled counter lives in Redis without being
// refreshed. The write-back loop persists to DB well within this window, and a
// lazy reload from DB on expiry keeps counts correct, so it is purely a memory
// hygiene measure, not a correctness boundary.
const counterTTL = 30 * 24 * time.Hour

// QuotaService implements the two-level (token + user) quota check and
// consumption with Redis acting as the real-time counter and PostgreSQL
// (used_quota) as the durable source of truth (Tech Design §4).
//
// Lazy-load semantics: a counter key is created on first access by reading the
// authoritative used_quota from the DB (SET), after which INCRBY tracks live
// usage. A background write-back loop periodically flushes the Redis counters to
// DB used_quota. After a restart — even one that wipes Redis — the counters are
// rebuilt from DB used_quota on next access, so usage is never lost.
//
// When rdb is nil the service degrades to operating directly against the DB
// used_quota columns, so quota enforcement still works without Redis.
type QuotaService struct {
	db  *gorm.DB
	rdb *redis.Client

	// dirty tracks which counters changed since the last write-back so the loop
	// only persists what moved. Keyed by entity id.
	mu          sync.Mutex
	dirtyTokens map[uint]struct{}
	dirtyUsers  map[uint]struct{}

	stop chan struct{}
}

// NewQuotaService constructs a QuotaService. rdb may be nil (DB-only mode).
func NewQuotaService(db *gorm.DB, rdb *redis.Client) *QuotaService {
	return &QuotaService{
		db:          db,
		rdb:         rdb,
		dirtyTokens: make(map[uint]struct{}),
		dirtyUsers:  make(map[uint]struct{}),
		stop:        make(chan struct{}),
	}
}

func tokenUsedKey(tokenID uint) string { return fmt.Sprintf("%s%d", tokenUsedKeyPrefix, tokenID) }
func userUsedKey(userID uint) string   { return fmt.Sprintf("%s%d", userUsedKeyPrefix, userID) }

// CheckRemaining returns the number of tokens the (token,user) pair may still
// consume: min(token remaining, user remaining), where a quota of -1 at either
// level is treated as unlimited. A nil error with a non-negative result means
// estTokens fits when result >= estTokens. The estTokens argument is the
// caller's pre-request estimate; this method does not reserve it, it only
// reports headroom so the relay (T7) can decide to admit or reject (402).
//
// The returned remaining is capped at math-safe values: when both levels are
// unlimited it returns a large sentinel (QuotaUnlimited is -1, but for
// "remaining" we return -1 to signal unlimited so callers can special-case it).
func (s *QuotaService) CheckRemaining(ctx context.Context, tokenID, userID uint, estTokens int64) (int64, error) {
	tokenQuota, tokenUsed, err := s.tokenQuotaAndUsed(ctx, tokenID)
	if err != nil {
		return 0, err
	}
	userQuota, userUsed, err := s.userQuotaAndUsed(ctx, userID)
	if err != nil {
		return 0, err
	}

	tokenRemaining := remaining(tokenQuota, tokenUsed)
	userRemaining := remaining(userQuota, userUsed)

	return minRemaining(tokenRemaining, userRemaining), nil
}

// HasRemaining is a convenience wrapper: it reports whether at least estTokens
// of headroom exists across both levels (unlimited always passes).
func (s *QuotaService) HasRemaining(ctx context.Context, tokenID, userID uint, estTokens int64) (bool, error) {
	rem, err := s.CheckRemaining(ctx, tokenID, userID, estTokens)
	if err != nil {
		return false, err
	}
	if rem < 0 {
		return true, nil // unlimited
	}
	return rem >= estTokens, nil
}

// Consume records actualTokens of usage against both the token and the user
// counters (Redis INCRBY when available, else a direct DB increment), and marks
// the counters dirty for asynchronous write-back. Consuming a non-positive
// amount is a no-op.
func (s *QuotaService) Consume(ctx context.Context, tokenID, userID uint, actualTokens int64) error {
	if actualTokens <= 0 {
		return nil
	}

	if s.rdb == nil {
		// DB-only mode: increment used_quota columns atomically.
		if err := s.db.Model(&model.Token{}).Where("id = ?", tokenID).
			UpdateColumn("used_quota", gorm.Expr("used_quota + ?", actualTokens)).Error; err != nil {
			return err
		}
		return s.db.Model(&model.User{}).Where("id = ?", userID).
			UpdateColumn("used_quota", gorm.Expr("used_quota + ?", actualTokens)).Error
	}

	// Ensure both counters are backfilled from DB before incrementing so the
	// INCRBY builds on the authoritative base (lazy-load).
	if _, _, err := s.tokenQuotaAndUsed(ctx, tokenID); err != nil {
		return err
	}
	if _, _, err := s.userQuotaAndUsed(ctx, userID); err != nil {
		return err
	}

	if err := s.rdb.IncrBy(ctx, tokenUsedKey(tokenID), actualTokens).Err(); err != nil {
		return err
	}
	if err := s.rdb.IncrBy(ctx, userUsedKey(userID), actualTokens).Err(); err != nil {
		return err
	}

	s.markDirty(tokenID, userID)
	return nil
}

// ResetUserUsage zeroes a user's consumed quota at both levels of truth: the
// durable DB used_quota column and the real-time Redis counter
// (quota:user:used:{userID}). It is the single canonical entry point for
// resetting user-level usage so the admin "reset_used" action and user deletion
// stay consistent with the live counter (Tech Design §2.5) — callers must not
// DEL the key directly elsewhere.
//
// It also drops any pending dirty flag for the user so the asynchronous
// write-back loop cannot resurrect the pre-reset count from a stale snapshot.
// With Redis configured the key is deleted (the next access lazily reseeds it
// from the now-zero DB value); without Redis the DB update alone suffices.
func (s *QuotaService) ResetUserUsage(ctx context.Context, userID uint) error {
	if err := s.db.Model(&model.User{}).Where("id = ?", userID).
		UpdateColumn("used_quota", 0).Error; err != nil {
		return err
	}

	// Forget any pending write-back for this user; the value we just wrote is the
	// new truth and a queued flush would otherwise overwrite it with a stale read.
	s.mu.Lock()
	delete(s.dirtyUsers, userID)
	s.mu.Unlock()

	if s.rdb == nil {
		return nil
	}
	// Delete rather than SET 0 so the lazy-load path reseeds from the (now zero)
	// DB value on next access, matching how a cold counter is initialised.
	if err := s.rdb.Del(ctx, userUsedKey(userID)).Err(); err != nil {
		return err
	}
	return nil
}

// markDirty flags both counters for the next write-back pass.
func (s *QuotaService) markDirty(tokenID, userID uint) {
	s.mu.Lock()
	s.dirtyTokens[tokenID] = struct{}{}
	s.dirtyUsers[userID] = struct{}{}
	s.mu.Unlock()
}

// tokenQuotaAndUsed returns the token's quota and its current used count. The
// used count comes from the Redis counter, lazily backfilled from DB used_quota
// when the key is missing (e.g. after a restart that cleared Redis).
func (s *QuotaService) tokenQuotaAndUsed(ctx context.Context, tokenID uint) (quota, used int64, err error) {
	var tok model.Token
	if err := s.db.Select("quota", "used_quota").First(&tok, tokenID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, 0, ErrQuotaTokenNotFound
		}
		return 0, 0, err
	}
	used, err = s.usedCounter(ctx, tokenUsedKey(tokenID), tok.UsedQuota)
	if err != nil {
		return 0, 0, err
	}
	return tok.Quota, used, nil
}

// userQuotaAndUsed mirrors tokenQuotaAndUsed for the user level.
func (s *QuotaService) userQuotaAndUsed(ctx context.Context, userID uint) (quota, used int64, err error) {
	var u model.User
	if err := s.db.Select("quota", "used_quota").First(&u, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, 0, ErrQuotaUserNotFound
		}
		return 0, 0, err
	}
	used, err = s.usedCounter(ctx, userUsedKey(userID), u.UsedQuota)
	if err != nil {
		return 0, 0, err
	}
	return u.Quota, used, nil
}

// usedCounter returns the live used count for a Redis key, lazily seeding it
// from dbUsed (the authoritative DB value) when the key is absent. With no Redis
// configured it returns dbUsed directly.
func (s *QuotaService) usedCounter(ctx context.Context, key string, dbUsed int64) (int64, error) {
	if s.rdb == nil {
		return dbUsed, nil
	}

	v, err := s.rdb.Get(ctx, key).Int64()
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, redis.Nil) {
		return 0, err
	}

	// Lazy-load: key missing -> seed from DB used_quota, then read back. SetNX
	// avoids clobbering a concurrent writer that beat us to the backfill.
	if err := s.rdb.SetNX(ctx, key, dbUsed, counterTTL).Err(); err != nil {
		return 0, err
	}
	v, err = s.rdb.Get(ctx, key).Int64()
	if err != nil {
		return 0, err
	}
	return v, nil
}

// remaining computes quota - used, treating quota == -1 (unlimited) as a
// negative sentinel (-1) so minRemaining can prefer the bounded level. A
// bounded remaining is clamped to >= 0.
func remaining(quota, used int64) int64 {
	if quota < 0 {
		return -1 // unlimited
	}
	r := quota - used
	if r < 0 {
		return 0
	}
	return r
}

// minRemaining returns the smaller of two "remaining" values where -1 means
// unlimited. min(unlimited, x) = x; min(unlimited, unlimited) = unlimited (-1).
func minRemaining(a, b int64) int64 {
	switch {
	case a < 0 && b < 0:
		return -1
	case a < 0:
		return b
	case b < 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

// StartWriteBack launches the periodic write-back loop that flushes dirty Redis
// counters to DB used_quota every interval. It is a no-op (returns immediately)
// when Redis is not configured. Call StopWriteBack to terminate it.
func (s *QuotaService) StartWriteBack(interval time.Duration) {
	if s.rdb == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				// Final flush on shutdown so the latest counts are durable.
				_ = s.WriteBack(context.Background())
				return
			case <-ticker.C:
				_ = s.WriteBack(context.Background())
			}
		}
	}()
}

// StopWriteBack signals the write-back loop to stop (and perform a final flush).
// Safe to call once; subsequent calls panic on a closed channel, so callers
// should invoke it a single time at shutdown.
func (s *QuotaService) StopWriteBack() {
	close(s.stop)
}

// WriteBack flushes the current Redis counter values for all dirty entities into
// the DB used_quota columns. It snapshots and clears the dirty sets first so new
// consumption during the flush is captured on the next pass. Counters are read
// from Redis (the live truth) and written authoritatively to DB.
func (s *QuotaService) WriteBack(ctx context.Context) error {
	if s.rdb == nil {
		return nil
	}

	s.mu.Lock()
	tokens := make([]uint, 0, len(s.dirtyTokens))
	for id := range s.dirtyTokens {
		tokens = append(tokens, id)
	}
	users := make([]uint, 0, len(s.dirtyUsers))
	for id := range s.dirtyUsers {
		users = append(users, id)
	}
	s.dirtyTokens = make(map[uint]struct{})
	s.dirtyUsers = make(map[uint]struct{})
	s.mu.Unlock()

	var firstErr error
	for _, id := range tokens {
		v, err := s.rdb.Get(ctx, tokenUsedKey(id)).Int64()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				continue
			}
			if firstErr == nil {
				firstErr = err
			}
			s.requeueToken(id)
			continue
		}
		if err := s.db.Model(&model.Token{}).Where("id = ?", id).
			UpdateColumn("used_quota", v).Error; err != nil {
			if firstErr == nil {
				firstErr = err
			}
			s.requeueToken(id)
		}
	}
	for _, id := range users {
		v, err := s.rdb.Get(ctx, userUsedKey(id)).Int64()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				continue
			}
			if firstErr == nil {
				firstErr = err
			}
			s.requeueUser(id)
			continue
		}
		if err := s.db.Model(&model.User{}).Where("id = ?", id).
			UpdateColumn("used_quota", v).Error; err != nil {
			if firstErr == nil {
				firstErr = err
			}
			s.requeueUser(id)
		}
	}
	return firstErr
}

// requeueToken / requeueUser re-mark an entity dirty after a failed flush so the
// next pass retries it rather than dropping the pending write.
func (s *QuotaService) requeueToken(id uint) {
	s.mu.Lock()
	s.dirtyTokens[id] = struct{}{}
	s.mu.Unlock()
}

func (s *QuotaService) requeueUser(id uint) {
	s.mu.Lock()
	s.dirtyUsers[id] = struct{}{}
	s.mu.Unlock()
}
