package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/niktaninic/gmod-yt-backend/yt-converter-server/internal/config"
	"github.com/niktaninic/gmod-yt-backend/yt-converter-server/internal/media"
)

// identity describes who is calling.
//
//   - server: the GMod server, authenticated with X-SR-Key (= API_SECRET).
//     Player fields in the body are trusted.
//   - client: a GMod client with a short-lived HMAC token (X-SR-Token) that
//     the GMod server signed for it. SteamID and rank come from the token and
//     cannot be spoofed. This replaces replicating API_SECRET to every client.
type identity struct {
	server  bool
	steamID string
	rank    string
}

type ctxKey struct{}

func identityFrom(r *http.Request) identity {
	id, _ := r.Context().Value(ctxKey{}).(identity)
	return id
}

func (s *Server) checkAPIKey(key string) bool {
	if key == "" {
		return false
	}
	// hash both sides so the comparison is constant time regardless of length
	a := sha256.Sum256([]byte(key))
	b := sha256.Sum256([]byte(s.cfg.APISecret))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// clientTokenMAC is HMAC-SHA256(API_SECRET, "v1|steamid|rank|exp") as hex.
// The Lua side (autorun/streamradio_converter.lua) computes the same thing.
func clientTokenMAC(secret, payload string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

// verifyClientToken parses "v1|<steamid>|<rank>|<exp unix>|<hex mac>".
func (s *Server) verifyClientToken(tok string) (identity, bool) {
	if !s.cfg.ClientTokensEnabled || len(tok) > 256 {
		return identity{}, false
	}
	parts := strings.Split(tok, "|")
	if len(parts) != 5 || parts[0] != "v1" {
		return identity{}, false
	}
	exp, err := strconv.ParseInt(parts[3], 10, 64)
	now := time.Now().Unix()
	if err != nil || exp < now || exp > now+int64(s.cfg.ClientTokenMaxTTL/time.Second) {
		return identity{}, false
	}
	if !media.ValidSteamID(parts[1]) || !config.ValidRank(parts[2]) {
		return identity{}, false
	}
	want := clientTokenMAC(s.cfg.APISecret, strings.Join(parts[:4], "|"))
	if !hmac.Equal([]byte(want), []byte(strings.ToLower(parts[4]))) {
		return identity{}, false
	}
	return identity{steamID: parts[1], rank: parts[2]}, true
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := s.clientIP(r)

		var id identity
		ok := false
		if s.checkAPIKey(r.Header.Get("X-SR-Key")) {
			id, ok = identity{server: true}, true
		} else if tok := r.Header.Get("X-SR-Token"); tok != "" {
			id, ok = s.verifyClientToken(tok)
		}

		// flood protection for everything except the trusted GMod server
		if !id.server {
			if !s.floodLimiter.Allow("ip:"+ip, 240) {
				writeRetryAfter(w, 10*time.Second)
				writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests, please try again later")
				return
			}
		}
		if !ok {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "Missing or invalid authentication. Send X-SR-Key (server) or X-SR-Token (client).")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
	}
}

func (s *Server) serverOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !identityFrom(r).server {
			writeError(w, http.StatusForbidden, "FORBIDDEN", "This endpoint requires the server key")
			return
		}
		next(w, r)
	}
}

// workKey picks the rate-limit bucket for expensive work (fresh conversions,
// metadata lookups). Requests are keyed per player when we know one; the old
// code keyed on a client-supplied server_ip, which anyone could rotate.
func (s *Server) workKey(id identity, ip string) (string, int) {
	if id.steamID != "" {
		return "p:" + id.steamID, s.cfg.RateLimitMax
	}
	if id.server {
		return "s:" + ip, s.cfg.RateLimitServerMax
	}
	return "i:" + ip, s.cfg.RateLimitMax
}

// ---- token-bucket limiter set ----------------------------------------------------

type limiterSet struct {
	window time.Duration
	mu     sync.Mutex
	m      map[string]*limEntry
}

type limEntry struct {
	l    *rate.Limiter
	seen time.Time
}

func newLimiterSet(window time.Duration) *limiterSet {
	return &limiterSet{window: window, m: map[string]*limEntry{}}
}

// Allow permits `max` events per window with bursts up to `max`.
func (l *limiterSet) Allow(key string, max int) bool {
	if max <= 0 {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	e := l.m[key]
	if e == nil {
		e = &limEntry{l: rate.NewLimiter(rate.Every(l.window/time.Duration(max)), max)}
		l.m[key] = e
	}
	e.seen = now
	l.mu.Unlock()
	return e.l.AllowN(now, 1)
}

func (l *limiterSet) gcLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			l.mu.Lock()
			for k, e := range l.m {
				if now.Sub(e.seen) > 2*l.window {
					delete(l.m, k)
				}
			}
			l.mu.Unlock()
		}
	}
}
