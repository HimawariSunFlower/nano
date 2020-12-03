package env

import (
	"container/list"
	"time"
)

type RateLimitingMaker struct {
	make func() *RateLimiter
}

func NewRateLimitingMaker(limit int, interval time.Duration) *RateLimitingMaker {
	r := &RateLimitingMaker{}

	r.make = func() *RateLimiter {
		_r := &RateLimiter{
			limit:    limit,
			interval: interval,
		}

		_r.times.Init()

		return _r
	}

	return r
}

func NewRateLimiter(cfg *RateLimitingMaker) *RateLimiter {
	if cfg == nil {
		return nil
	}

	return cfg.make()
}

type RateLimiter struct {
	limit    int
	interval time.Duration
	times    list.List
}

func (r *RateLimiter) ShouldRateLimit(now time.Time) bool {
	if r.times.Len() < r.limit {
		r.times.PushBack(now)
		return false
	}

	front := r.times.Front()
	if diff := now.Sub(front.Value.(time.Time)); diff < r.interval {
		return true
	}

	front.Value = now
	r.times.MoveToBack(front)
	return false
}
