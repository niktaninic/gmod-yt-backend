// Package spotify is a minimal Spotify Web API client (client-credentials).
package spotify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Track struct {
	ID       string
	Name     string
	Artists  string
	Duration int // seconds
}

func (t Track) Title() string {
	if t.Artists == "" {
		return t.Name
	}
	return t.Artists + " - " + t.Name
}

type Client struct {
	id, secret string
	http       *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time

	trackMu    sync.Mutex
	trackCache map[string]trackCacheItem
}

type trackCacheItem struct {
	t   Track
	exp time.Time
}

type APIError struct{ Status int }

func (e *APIError) Error() string { return fmt.Sprintf("spotify API returned HTTP %d", e.Status) }

func New(id, secret string) *Client {
	return &Client{
		id:     id,
		secret: secret,
		http: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     90 * time.Second,
				ForceAttemptHTTP2:   true,
			},
		},
		trackCache: map[string]trackCacheItem{},
	}
}

// getToken is serialised so concurrent callers don't all request a new token
// (old code raced here).
func (c *Client) getToken(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.token != "" && time.Now().Before(c.tokenExp) {
		return c.token, nil
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://accounts.spotify.com/api/token",
		strings.NewReader("grant_type=client_credentials"))
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.id+":"+c.secret)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("spotify auth failed (HTTP %d)", resp.StatusCode)
	}
	var data struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&data); err != nil {
		return "", err
	}
	c.token = data.AccessToken
	ttl := time.Duration(data.ExpiresIn-60) * time.Second
	if ttl < time.Minute {
		ttl = time.Minute
	}
	c.tokenExp = time.Now().Add(ttl)
	return c.token, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := c.getToken(ctx, attempt > 0)
		if err != nil {
			return err
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.spotify.com"+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			continue // token revoked early, refresh once
		}
		if resp.StatusCode != http.StatusOK {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			return &APIError{Status: resp.StatusCode}
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
		resp.Body.Close()
		return err
	}
	return errors.New("spotify: unauthorized")
}

type apiTrack struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	DurationMS int    `json:"duration_ms"`
	Artists    []struct {
		Name string `json:"name"`
	} `json:"artists"`
}

func (t apiTrack) toTrack() Track {
	names := make([]string, 0, len(t.Artists))
	for _, a := range t.Artists {
		if a.Name != "" {
			names = append(names, a.Name)
		}
	}
	return Track{ID: t.ID, Name: t.Name, Artists: strings.Join(names, ", "), Duration: t.DurationMS / 1000}
}

func (c *Client) GetTrack(ctx context.Context, id string) (Track, error) {
	c.trackMu.Lock()
	if it, ok := c.trackCache[id]; ok && time.Now().Before(it.exp) {
		c.trackMu.Unlock()
		return it.t, nil
	}
	c.trackMu.Unlock()

	var t apiTrack
	if err := c.get(ctx, "/v1/tracks/"+url.PathEscape(id), &t); err != nil {
		return Track{}, err
	}
	tr := t.toTrack()

	c.trackMu.Lock()
	if len(c.trackCache) > 4096 {
		c.trackCache = map[string]trackCacheItem{}
	}
	c.trackCache[id] = trackCacheItem{t: tr, exp: time.Now().Add(6 * time.Hour)}
	c.trackMu.Unlock()
	return tr, nil
}

// PlaylistTracks pages through a playlist, asking only for the fields we use.
func (c *Client) PlaylistTracks(ctx context.Context, id string, max int) ([]Track, error) {
	const page = 100
	fields := url.QueryEscape("next,items(track(id,name,duration_ms,artists(name)),item(id,name,duration_ms,artists(name)))")
	var out []Track
	endpoint := "tracks"
	for offset := 0; ; offset += page {
		var data struct {
			Next  *string `json:"next"`
			Items []struct {
				Track *apiTrack `json:"track"`
				Item  *apiTrack `json:"item"`
			} `json:"items"`
		}
		path := fmt.Sprintf("/v1/playlists/%s/%s?limit=%d&offset=%d&fields=%s", url.PathEscape(id), endpoint, page, offset, fields)
		err := c.get(ctx, path, &data)
		var apiErr *APIError
		if err != nil && offset == 0 && endpoint == "tracks" && errors.As(err, &apiErr) && (apiErr.Status == 404 || apiErr.Status == 410) {
			// newer API revisions renamed /tracks to /items
			endpoint = "items"
			offset -= page
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, it := range data.Items {
			t := it.Track
			if t == nil {
				t = it.Item
			}
			if t == nil || t.ID == "" {
				continue
			}
			out = append(out, t.toTrack())
			if max > 0 && len(out) >= max {
				return out, nil
			}
		}
		if data.Next == nil || *data.Next == "" || len(data.Items) == 0 {
			break
		}
	}
	return out, nil
}
