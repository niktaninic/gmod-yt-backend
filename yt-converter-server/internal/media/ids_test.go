package media

import "testing"

func TestYouTubeVideoID(t *testing.T) {
	cases := map[string]string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ":               "dQw4w9WgXcQ",
		"https://www.youtube.com/watch?feature=share&v=dQw4w9WgXcQ": "dQw4w9WgXcQ",
		"https://youtu.be/dQw4w9WgXcQ?t=42":                         "dQw4w9WgXcQ",
		"https://m.youtube.com/watch?v=dQw4w9WgXcQ&list=PLx":        "dQw4w9WgXcQ",
		"https://music.youtube.com/watch?v=dQw4w9WgXcQ":             "dQw4w9WgXcQ",
		"https://www.youtube.com/shorts/dQw4w9WgXcQ":                "dQw4w9WgXcQ",
		"https://www.youtube.com/live/dQw4w9WgXcQ?si=abc":           "dQw4w9WgXcQ",
		"https://www.youtube-nocookie.com/embed/dQw4w9WgXcQ":        "dQw4w9WgXcQ",
		"dQw4w9WgXcQ": "dQw4w9WgXcQ",
		"https://www.youtube.com/watch?v=dQw4w9WgXcQextra":            "",
		"https://example.com/watch?v=dQw4w9WgXcQ":                     "",
		"https://www.youtube.com/playlist?list=PLxxxxxxxxxxxxxxxxxxx": "",
	}
	for in, want := range cases {
		if got := YouTubeVideoID(in); got != want {
			t.Errorf("YouTubeVideoID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlaylistIDs(t *testing.T) {
	if got := YouTubePlaylistID("https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=PL123abc"); got != "PL123abc" {
		t.Errorf("yt playlist: %q", got)
	}
	if got := YouTubePlaylistID("https://example.com/?list=PL123abc"); got != "" {
		t.Errorf("non-youtube playlist matched: %q", got)
	}
	if got := SpotifyPlaylistID("https://open.spotify.com/intl-de/playlist/37i9dQZF1DXcBWIGoYBM5M?si=x"); got != "37i9dQZF1DXcBWIGoYBM5M" {
		t.Errorf("spotify playlist: %q", got)
	}
	if got := SpotifyTrackID("spotify:track:4uLU6hMCjMI75M1A2tKUQC"); got != "4uLU6hMCjMI75M1A2tKUQC" {
		t.Errorf("spotify track: %q", got)
	}
}

func TestSteamIDAndIP(t *testing.T) {
	for _, ok := range []string{"STEAM_0:1:12345", "[U:1:24691]", "76561197960290418"} {
		if !ValidSteamID(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "BOT", "STEAM_0:2:1", "STEAM_0:1:1; DROP TABLE", "7656119"} {
		if ValidSteamID(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
	if ValidIP("1.2.3.4:27015") || !ValidIP("1.2.3.4") || !ValidIP("::1") {
		t.Error("ValidIP")
	}
	if got := CleanText("a\x00b\nc", 2); got != "ab" {
		t.Errorf("CleanText = %q", got)
	}
}
