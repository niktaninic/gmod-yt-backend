package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/niktaninic/gmod-yt-backend/yt-converter-server/internal/config"
	"github.com/niktaninic/gmod-yt-backend/yt-converter-server/internal/ytdlp"
)

func testServer() *Server {
	return &Server{cfg: &config.Config{APISecret: "secret-for-tests-123", ClientTokensEnabled: true, ClientTokenMaxTTL: 24 * time.Hour}}
}

func token(secret, steamid, rank string, exp int64) string {
	p := fmt.Sprintf("v1|%s|%s|%d", steamid, rank, exp)
	return p + "|" + clientTokenMAC(secret, p)
}

func TestClientToken(t *testing.T) {
	s := testServer()
	now := time.Now().Unix()

	id, ok := s.verifyClientToken(token(s.cfg.APISecret, "STEAM_0:1:42", "vip", now+3600))
	if !ok || id.steamID != "STEAM_0:1:42" || id.rank != "vip" || id.server {
		t.Fatalf("valid token rejected: %+v %v", id, ok)
	}
	bad := []string{
		token("wrong-secret", "STEAM_0:1:42", "vip", now+3600),
		token(s.cfg.APISecret, "STEAM_0:1:42", "vip", now-1),      // expired
		token(s.cfg.APISecret, "STEAM_0:1:42", "vip", now+90000),  // too far in the future
		token(s.cfg.APISecret, "not-a-steamid", "vip", now+3600),  // bad steamid
		token(s.cfg.APISecret, "STEAM_0:1:42", "v i p", now+3600), // bad rank
		"v1|STEAM_0:1:42|vip",
		"",
	}
	for _, b := range bad {
		if _, ok := s.verifyClientToken(b); ok {
			t.Errorf("accepted bad token %q", b)
		}
	}
	// HMAC test vector shared with the Lua self-test
	if got := clientTokenMAC("key", "The quick brown fox jumps over the lazy dog"); got != "f7bc83f430538424b13298e6aa6fb143ef4d59a14946175997479dbc2d1a3cd8" {
		t.Errorf("hmac vector: %s", got)
	}
}

func TestAPIKey(t *testing.T) {
	s := testServer()
	if !s.checkAPIKey("secret-for-tests-123") || s.checkAPIKey("secret-for-tests-12") || s.checkAPIKey("") {
		t.Fatal("checkAPIKey")
	}
}

func TestPickBestMatch(t *testing.T) {
	res := []ytdlp.Info{
		{ID: "a", Duration: 3600}, // 1h loop, top result
		{ID: "b", Duration: 215},  // close to spotify length
		{ID: "c", Duration: 213, IsLive: true},
	}
	if got := pickBestMatch(res, 213); got.ID != "b" {
		t.Errorf("picked %s", got.ID)
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiterSet(time.Minute)
	for i := 0; i < 3; i++ {
		if !l.Allow("k", 3) {
			t.Fatalf("request %d denied", i)
		}
	}
	if l.Allow("k", 3) {
		t.Fatal("4th request allowed")
	}
	if !l.Allow("other", 3) {
		t.Fatal("separate key denied")
	}
}

func TestFilterPlaylist(t *testing.T) {
	in := []playlistTrack{
		{VideoID: "a", Title: "ok", Duration: 100},
		{VideoID: "b", Title: "[Deleted video]"},
		{VideoID: "c", Title: "long", Duration: 5000},
		{VideoID: "d", Title: "unknown length", Duration: 0},
	}
	out, skipped := filterPlaylist(in, 600)
	if len(out) != 2 || skipped != 2 || out[0].VideoID != "a" || out[1].VideoID != "d" {
		t.Fatalf("got %+v skipped=%d", out, skipped)
	}
	if out, _ := filterPlaylist(in, 0); len(out) != 3 {
		t.Fatalf("unlimited rank should keep long tracks, got %d", len(out))
	}
}
