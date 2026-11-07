package api

import (
	"sync"
	"time"
)

// Throttle 限制登录失败次数:一个键(用户名或来源 IP)在 Window 内失败 Max 次,就锁定 Lock 这么久,期间不再放行。
// 状态在内存里、每个副本各一份,所以多副本时有效上限是 Max × 副本数;重启会清零。
type Throttle struct {
	Max    int
	Window time.Duration
	Lock   time.Duration
	Now    func() time.Time

	mu sync.Mutex
	m  map[string]*entry
}

type entry struct {
	fails       int
	first       time.Time
	lockedUntil time.Time
}

func NewThrottle(max int, window, lock time.Duration) *Throttle {
	return &Throttle{Max: max, Window: window, Lock: lock, Now: time.Now, m: map[string]*entry{}}
}

// Allow 报告这个键现在能不能尝试;被锁定时返回还要等多久。
func (t *Throttle) Allow(key string) (bool, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.gc()
	e := t.m[key]
	if e == nil {
		return true, 0
	}
	if now := t.Now(); now.Before(e.lockedUntil) {
		return false, e.lockedUntil.Sub(now)
	}
	return true, 0
}

// Fail 记一次失败;达到上限就锁定。
func (t *Throttle) Fail(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.Now()
	e := t.m[key]
	if e == nil || now.Sub(e.first) > t.Window {
		e = &entry{first: now}
		t.m[key] = e
	}
	e.fails++
	if e.fails >= t.Max {
		e.lockedUntil = now.Add(t.Lock)
		e.fails, e.first = 0, now // 锁定结束后重新计数
	}
}

// Reset 在成功登录后清掉这个键的记录。
func (t *Throttle) Reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, key)
}

// gc 清掉已经过期的记录,避免被人用大量不同的用户名撑大内存。调用前要持有锁。
func (t *Throttle) gc() {
	if len(t.m) < 1024 {
		return
	}
	now := t.Now()
	for k, e := range t.m {
		if now.After(e.lockedUntil) && now.Sub(e.first) > t.Window {
			delete(t.m, k)
		}
	}
}
