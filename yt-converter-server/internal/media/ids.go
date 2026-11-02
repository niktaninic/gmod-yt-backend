// Package media parses YouTube / Spotify URLs and player identifiers.
package media

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

var (
	videoIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{11}$`)
	// covers watch?v=, youtu.be/, /embed/, /v/, /shorts/, /live/, music./m. subdomains
	ytPathRe     = regexp.MustCompile(`(?i)(?:youtu\.be/|youtube(?:-nocookie)?\.com/(?:embed/|v/|shorts/|live/|e/))([a-zA-Z0-9_-]{11})(?:[^a-zA-Z0-9_-]|$)`)
	ytQueryRe    = regexp.MustCompile(`(?i)youtube\.com/.*[?&]v=([a-zA-Z0-9_-]{11})(?:[^a-zA-Z0-9_-]|$)`)
	ytListRe     = regexp.MustCompile(`[?&]list=([a-zA-Z0-9_-]{2,64})`)
	spTrackRe    = regexp.MustCompile(`(?i)(?:open\.spotify\.com/(?:intl-[a-z]{2}(?:-[a-z]{2})?/)?(?:embed/)?track/|spotify:track:)([a-zA-Z0-9]{22})`)
	spPlaylistRe = regexp.MustCompile(`(?i)(?:open\.spotify\.com/(?:intl-[a-z]{2}(?:-[a-z]{2})?/)?(?:embed/)?playlist/|spotify:playlist:)([a-zA-Z0-9]{22})`)

	steamLegacyRe = regexp.MustCompile(`^STEAM_[0-5]:[01]:\d{1,10}$`)
	steam3Re      = regexp.MustCompile(`^\[U:1:\d{1,10}\]$`)
	steam64Re     = regexp.MustCompile(`^7656119\d{10}$`)
)

// ValidVideoID reports whether id looks like a YouTube video id.
func ValidVideoID(id string) bool { return videoIDRe.MatchString(id) }

// YouTubeVideoID extracts a video id from a URL or a bare id.
func YouTubeVideoID(s string) string {
	s = strings.TrimSpace(s)
	if videoIDRe.MatchString(s) {
		return s
	}
	if m := ytQueryRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	if m := ytPathRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// YouTubePlaylistID returns the list= parameter of a YouTube URL.
func YouTubePlaylistID(s string) string {
	if !strings.Contains(strings.ToLower(s), "youtu") {
		return ""
	}
	if m := ytListRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

func SpotifyTrackID(s string) string {
	if m := spTrackRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

func SpotifyPlaylistID(s string) string {
	if m := spPlaylistRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// ValidSteamID accepts STEAM_X:Y:Z, [U:1:Z] and SteamID64.
func ValidSteamID(s string) bool {
	if len(s) == 0 || len(s) > 25 {
		return false
	}
	return steamLegacyRe.MatchString(s) || steam3Re.MatchString(s) || steam64Re.MatchString(s)
}

// ValidIP accepts a literal IPv4/IPv6 address (used only for logging).
func ValidIP(s string) bool {
	if s == "" || len(s) > 45 {
		return false
	}
	return net.ParseIP(s) != nil
}

// CleanText trims and truncates free-form text (nicknames etc.) and strips
// control characters so it is safe to log.
func CleanText(s string, max int) string {
	s = strings.TrimSpace(s)
	b := strings.Builder{}
	n := 0
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		if n >= max {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// IsHTTPURL is a cheap sanity check before handing a string to yt-dlp.
func IsHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// FormatDuration renders seconds as m:ss.
func FormatDuration(sec int) string {
	if sec < 0 {
		sec = 0
	}
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}
