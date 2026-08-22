package gateway

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"sync"
	"time"

	"shepard/internal/config"
)

type admissionScope struct {
	key    string
	limits config.Limits
}

type bucketState struct {
	tokens  float64
	updated time.Time
}

type limitStore struct {
	mu         sync.Mutex
	buckets    map[string]bucketState
	concurrent map[string]int
}

func newLimitStore() *limitStore {
	return &limitStore{
		buckets:    make(map[string]bucketState),
		concurrent: make(map[string]int),
	}
}

func (s *limitStore) clear() {
	s.mu.Lock()
	// Keep active concurrency reservations: their release closures still refer
	// to this map. Only rate history is configuration-dependent and reset here.
	s.buckets = make(map[string]bucketState)
	s.mu.Unlock()
}

func (s *limitStore) admit(scopes ...admissionScope) (func(), bool) {
	// Check all scopes under one lock before consuming anything. This makes a
	// multi-scope admission (for example ingress + client) all-or-nothing.
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	states := make(map[string]bucketState, len(scopes))
	for _, scope := range scopes {
		if scope.limits.MaxConcurrent > 0 && s.concurrent[scope.key] >= scope.limits.MaxConcurrent {
			return nil, false
		}
		if scope.limits.RequestsPerMinute <= 0 {
			continue
		}
		state, exists := s.buckets[scope.key]
		if !exists {
			state = bucketState{tokens: float64(scope.limits.Burst), updated: now}
		} else {
			elapsed := now.Sub(state.updated).Minutes()
			state.tokens += elapsed * scope.limits.RequestsPerMinute
			if maximum := float64(scope.limits.Burst); state.tokens > maximum {
				state.tokens = maximum
			}
			state.updated = now
		}
		states[scope.key] = state
		if state.tokens < 1 {
			s.buckets[scope.key] = state
			return nil, false
		}
	}
	for _, scope := range scopes {
		if scope.limits.RequestsPerMinute > 0 {
			state := states[scope.key]
			state.tokens--
			s.buckets[scope.key] = state
		}
		if scope.limits.MaxConcurrent > 0 {
			s.concurrent[scope.key]++
		}
	}
	// Callers hold concurrency capacity until this release function is invoked
	// exactly once, normally through defer.
	return func() {
		s.mu.Lock()
		for _, scope := range scopes {
			if scope.limits.MaxConcurrent > 0 && s.concurrent[scope.key] > 0 {
				s.concurrent[scope.key]--
				if s.concurrent[scope.key] == 0 {
					delete(s.concurrent, scope.key)
				}
			}
		}
		s.mu.Unlock()
	}, true
}

func clientIdentity(r *http.Request, trusted *clientACL) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		// Group requests by credential without retaining the credential itself.
		digest := sha256.Sum256([]byte(auth))
		return fmt.Sprintf("key:%x", digest[:8])
	}
	if ip := forwardedClientIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"), trusted); ip != nil {
		return "ip:" + ip.String()
	}
	return "ip:" + r.RemoteAddr
}
