package helps

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
)

const (
	defaultUpstreamQueueSize               = 64
	defaultUpstreamQueueTimeout            = 10 * time.Minute
	defaultUpstreamOverloadCooldown        = 5 * time.Second
	defaultUpstreamSuccessesBeforeIncrease = 8
)

var defaultUpstreamOverloadStatusCodes = []int{
	http.StatusRequestTimeout,
	http.StatusTooManyRequests,
	http.StatusBadGateway,
	http.StatusServiceUnavailable,
	http.StatusGatewayTimeout,
}

type upstreamConcurrencySettings struct {
	enabled                 bool
	minConcurrency          int
	initialConcurrency      int
	maxConcurrency          int
	queueSize               int
	queueTimeout            time.Duration
	overloadCooldown        time.Duration
	successesBeforeIncrease int
	overloadStatusCodes     map[int]struct{}
	convertOverloadTo429    bool
}

type upstreamConcurrencyController struct {
	mu            sync.Mutex
	key           string
	initialized   bool
	active        int
	queued        int
	limit         int
	successStreak int
	epoch         uint64
	cooldownUntil time.Time
	settings      upstreamConcurrencySettings
	changed       chan struct{}
}

type UpstreamConcurrencyLease struct {
	controller *upstreamConcurrencyController
	epoch      uint64
	once       sync.Once
}

type UpstreamAdmissionError struct {
	message    string
	retryAfter time.Duration
}

var upstreamConcurrencyControllers sync.Map

func AcquireUpstreamConcurrency(
	ctx context.Context,
	key string,
	cfg *config.OpenAICompatibilityUpstreamConcurrency,
) (*UpstreamConcurrencyLease, error) {
	settings := normalizeUpstreamConcurrencySettings(cfg)
	if !settings.enabled {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	value, _ := upstreamConcurrencyControllers.LoadOrStore(key, &upstreamConcurrencyController{
		changed: make(chan struct{}),
		key:     key,
	})
	controller, ok := value.(*upstreamConcurrencyController)
	if !ok || controller == nil {
		return nil, fmt.Errorf("upstream concurrency controller unavailable")
	}
	return controller.acquire(ctx, settings)
}

func (e *UpstreamAdmissionError) Error() string {
	if e == nil {
		return "upstream admission unavailable"
	}
	return e.message
}

func (e *UpstreamAdmissionError) StatusCode() int { return http.StatusTooManyRequests }

func (e *UpstreamAdmissionError) RetryAfter() *time.Duration {
	if e == nil {
		return nil
	}
	retryAfter := e.retryAfter
	return &retryAfter
}

func (l *UpstreamConcurrencyLease) FinishSuccess() {
	if l == nil || l.controller == nil {
		return
	}
	l.once.Do(func() {
		l.controller.release(upstreamConcurrencySuccess, l.epoch)
	})
}

func (l *UpstreamConcurrencyLease) FinishNeutral() {
	if l == nil || l.controller == nil {
		return
	}
	l.once.Do(func() {
		l.controller.release(upstreamConcurrencyNeutral, l.epoch)
	})
}

func (l *UpstreamConcurrencyLease) FinishTransportFailure() {
	if l == nil || l.controller == nil {
		return
	}
	l.once.Do(func() {
		l.controller.release(upstreamConcurrencyOverload, l.epoch)
	})
}

func (l *UpstreamConcurrencyLease) FinishHTTPFailure(statusCode int, message string) error {
	if l == nil || l.controller == nil {
		return nil
	}
	var converted error
	l.once.Do(func() {
		isOverload, convertTo429, retryAfter := l.controller.releaseHTTPFailure(statusCode, l.epoch)
		if isOverload && convertTo429 {
			converted = &UpstreamAdmissionError{
				message:    fmt.Sprintf("upstream overloaded with status %d: %s", statusCode, message),
				retryAfter: retryAfter,
			}
		}
	})
	return converted
}

type upstreamConcurrencyOutcome int

const (
	upstreamConcurrencyNeutral upstreamConcurrencyOutcome = iota
	upstreamConcurrencySuccess
	upstreamConcurrencyOverload
)

func (c *upstreamConcurrencyController) acquire(
	ctx context.Context,
	settings upstreamConcurrencySettings,
) (*UpstreamConcurrencyLease, error) {
	deadline := time.Now().Add(settings.queueTimeout)
	countedQueued := false
	for {
		c.mu.Lock()
		c.applySettingsLocked(settings)
		now := time.Now()
		coolingDown := now.Before(c.cooldownUntil)
		if !coolingDown && c.active < c.limit {
			if countedQueued {
				c.queued--
			}
			c.active++
			epoch := c.epoch
			c.mu.Unlock()
			return &UpstreamConcurrencyLease{controller: c, epoch: epoch}, nil
		}
		if !countedQueued {
			if c.queued >= c.settings.queueSize {
				retryAfter := c.retryAfterLocked(now)
				log.WithFields(log.Fields{
					"upstream": c.key,
					"active":   c.active,
					"queued":   c.queued,
					"limit":    c.limit,
				}).Warn("upstream admission queue is full")
				c.mu.Unlock()
				return nil, newUpstreamAdmissionError("upstream admission queue is full", retryAfter)
			}
			c.queued++
			countedQueued = true
		}
		changed := c.changed
		cooldownWait := time.Duration(0)
		if coolingDown {
			cooldownWait = c.cooldownUntil.Sub(now)
		}
		c.mu.Unlock()

		remaining := time.Until(deadline)
		if remaining <= 0 {
			c.removeQueuedWaiter()
			log.WithField("upstream", c.key).Warn("upstream admission queue timed out")
			return nil, newUpstreamAdmissionError("upstream admission queue timed out", settings.overloadCooldown)
		}
		wait := remaining
		if cooldownWait > 0 && cooldownWait < wait {
			wait = cooldownWait
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			stopUpstreamConcurrencyTimer(timer)
			c.removeQueuedWaiter()
			return nil, ctx.Err()
		case <-changed:
			stopUpstreamConcurrencyTimer(timer)
		case <-timer.C:
		}
	}
}

func (c *upstreamConcurrencyController) applySettingsLocked(settings upstreamConcurrencySettings) {
	c.settings = settings
	if !c.initialized {
		c.limit = settings.initialConcurrency
		c.initialized = true
	}
	if c.limit < settings.minConcurrency {
		c.limit = settings.minConcurrency
	}
	if c.limit > settings.maxConcurrency {
		c.limit = settings.maxConcurrency
	}
}

func (c *upstreamConcurrencyController) release(outcome upstreamConcurrencyOutcome, leaseEpoch uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active > 0 {
		c.active--
	}
	switch outcome {
	case upstreamConcurrencySuccess:
		if leaseEpoch == c.epoch {
			c.successStreak++
			if c.successStreak >= c.settings.successesBeforeIncrease && c.limit < c.settings.maxConcurrency {
				c.limit++
				c.successStreak = 0
				log.WithFields(log.Fields{
					"upstream": c.key,
					"limit":    c.limit,
				}).Debug("upstream concurrency limit increased")
			}
		}
	case upstreamConcurrencyOverload:
		c.applyOverloadLocked(time.Now())
	}
	c.signalLocked()
}

func (c *upstreamConcurrencyController) releaseHTTPFailure(statusCode int, leaseEpoch uint64) (bool, bool, time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active > 0 {
		c.active--
	}
	_, isOverload := c.settings.overloadStatusCodes[statusCode]
	if isOverload {
		c.applyOverloadLocked(time.Now())
	} else if leaseEpoch == c.epoch {
		c.successStreak = 0
	}
	retryAfter := c.settings.overloadCooldown
	convertTo429 := c.settings.convertOverloadTo429
	c.signalLocked()
	return isOverload, convertTo429, retryAfter
}

func (c *upstreamConcurrencyController) applyOverloadLocked(now time.Time) {
	previousLimit := c.limit
	c.epoch++
	c.successStreak = 0
	newLimit := c.limit / 2
	if newLimit < c.settings.minConcurrency {
		newLimit = c.settings.minConcurrency
	}
	c.limit = newLimit
	cooldownUntil := now.Add(c.settings.overloadCooldown)
	if cooldownUntil.After(c.cooldownUntil) {
		c.cooldownUntil = cooldownUntil
	}
	log.WithFields(log.Fields{
		"upstream":  c.key,
		"old_limit": previousLimit,
		"new_limit": c.limit,
		"cooldown":  c.settings.overloadCooldown,
	}).Warn("upstream overload reduced concurrency limit")
}

func (c *upstreamConcurrencyController) removeQueuedWaiter() {
	c.mu.Lock()
	if c.queued > 0 {
		c.queued--
	}
	c.signalLocked()
	c.mu.Unlock()
}

func (c *upstreamConcurrencyController) retryAfterLocked(now time.Time) time.Duration {
	if c.cooldownUntil.After(now) {
		return c.cooldownUntil.Sub(now)
	}
	return c.settings.overloadCooldown
}

func (c *upstreamConcurrencyController) signalLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func normalizeUpstreamConcurrencySettings(cfg *config.OpenAICompatibilityUpstreamConcurrency) upstreamConcurrencySettings {
	settings := upstreamConcurrencySettings{
		queueSize:               defaultUpstreamQueueSize,
		queueTimeout:            defaultUpstreamQueueTimeout,
		overloadCooldown:        defaultUpstreamOverloadCooldown,
		successesBeforeIncrease: defaultUpstreamSuccessesBeforeIncrease,
		convertOverloadTo429:    true,
	}
	if cfg == nil || !cfg.Enabled {
		return settings
	}
	settings.enabled = true
	settings.minConcurrency = cfg.MinConcurrency
	if settings.minConcurrency <= 0 {
		settings.minConcurrency = 1
	}
	settings.maxConcurrency = cfg.MaxConcurrency
	if settings.maxConcurrency < settings.minConcurrency {
		settings.maxConcurrency = settings.minConcurrency
	}
	settings.initialConcurrency = cfg.InitialConcurrency
	if settings.initialConcurrency < settings.minConcurrency {
		settings.initialConcurrency = settings.minConcurrency
	}
	if settings.initialConcurrency > settings.maxConcurrency {
		settings.initialConcurrency = settings.maxConcurrency
	}
	if cfg.QueueSize > 0 {
		settings.queueSize = cfg.QueueSize
	}
	if cfg.QueueTimeoutSeconds > 0 {
		settings.queueTimeout = time.Duration(cfg.QueueTimeoutSeconds) * time.Second
	}
	if cfg.OverloadCooldownSeconds > 0 {
		settings.overloadCooldown = time.Duration(cfg.OverloadCooldownSeconds) * time.Second
	}
	if cfg.SuccessesBeforeIncrease > 0 {
		settings.successesBeforeIncrease = cfg.SuccessesBeforeIncrease
	}
	settings.overloadStatusCodes = make(map[int]struct{})
	statusCodes := cfg.OverloadStatusCodes
	if len(statusCodes) == 0 {
		statusCodes = defaultUpstreamOverloadStatusCodes
	}
	for _, statusCode := range statusCodes {
		if statusCode >= 100 && statusCode <= 599 {
			settings.overloadStatusCodes[statusCode] = struct{}{}
		}
	}
	if cfg.ConvertOverloadTo429 != nil {
		settings.convertOverloadTo429 = *cfg.ConvertOverloadTo429
	}
	return settings
}

func newUpstreamAdmissionError(message string, retryAfter time.Duration) *UpstreamAdmissionError {
	if retryAfter <= 0 {
		retryAfter = time.Second
	}
	return &UpstreamAdmissionError{message: message, retryAfter: retryAfter}
}

func stopUpstreamConcurrencyTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}
