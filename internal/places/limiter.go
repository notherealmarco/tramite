package places

import (
	"sync"
	"time"
)

// Limiter caps each client's requests per minute and per day (fixed
// windows: the minute and the UTC day). A zero limit is no limit.
type Limiter struct {
	perMinute, perDay int
	now               func() time.Time

	mu      sync.Mutex
	clients map[string]*window
	pruned  int64 // day of the last prune
}

type window struct {
	minute, day     int64
	inMinute, inDay int
}

func NewLimiter(perMinute, perDay int, now func() time.Time) *Limiter {
	return &Limiter{perMinute: perMinute, perDay: perDay, now: now, clients: map[string]*window{}}
}

// Allow counts one request of client, or says how long until its next one
// is allowed (the end of the full window: the minute, or the UTC day).
func (l *Limiter) Allow(client string) (time.Duration, bool) {
	if l == nil || (l.perMinute == 0 && l.perDay == 0) {
		return 0, true
	}
	now := l.now()
	minute, day := now.Unix()/60, now.Unix()/86400
	l.mu.Lock()
	defer l.mu.Unlock()
	if day != l.pruned {
		l.pruned = day
		for k, w := range l.clients {
			if w.day != day {
				delete(l.clients, k)
			}
		}
	}
	w := l.clients[client]
	if w == nil {
		w = &window{}
		l.clients[client] = w
	}
	if w.day != day {
		w.day, w.inDay = day, 0
	}
	if w.minute != minute {
		w.minute, w.inMinute = minute, 0
	}
	var wait time.Duration
	if l.perDay > 0 && w.inDay >= l.perDay {
		wait = time.Unix((day+1)*86400, 0).Sub(now)
	} else if l.perMinute > 0 && w.inMinute >= l.perMinute {
		wait = time.Unix((minute+1)*60, 0).Sub(now)
	}
	if wait > 0 {
		return wait, false
	}
	w.inDay++
	w.inMinute++
	return 0, true
}
