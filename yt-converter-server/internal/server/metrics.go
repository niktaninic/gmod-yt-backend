package server

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxEvents = 500

type event struct {
	Type       string `json:"type"`
	VideoID    string `json:"videoId,omitempty"`
	Title      string `json:"title,omitempty"`
	Error      string `json:"error,omitempty"`
	Source     string `json:"source,omitempty"`
	Nick       string `json:"nick,omitempty"`
	SteamID    string `json:"steamid,omitempty"`
	ServerIP   string `json:"serverIp,omitempty"`
	PlayerIP   string `json:"playerIp,omitempty"`
	DurationMs int64  `json:"durationMs"`
	YtdlpMs    int64  `json:"ytdlpMs,omitempty"`
	Ts         int64  `json:"ts"`
}

// Metrics keeps counters and a fixed-size ring buffer of recent events
// (the old JS version used Array.shift(), O(n) per insert).
type Metrics struct {
	conversions     atomic.Int64
	cacheHits       atomic.Int64
	errors          atomic.Int64
	spotifyResolves atomic.Int64
	apiCalls        atomic.Int64
	activeStreams   atomic.Int64
	activeConv      atomic.Int64

	mu     sync.Mutex
	ring   [maxEvents]event
	next   int
	filled bool

	started  time.Time
	cpuModel string
}

func newMetrics() *Metrics {
	return &Metrics{started: time.Now(), cpuModel: readCPUModel()}
}

func (m *Metrics) push(e event) {
	e.Ts = time.Now().UnixMilli()
	m.mu.Lock()
	m.ring[m.next] = e
	m.next = (m.next + 1) % maxEvents
	if m.next == 0 {
		m.filled = true
	}
	m.mu.Unlock()
}

func (m *Metrics) recordConversion(videoID, title, source string, c caller, cached bool, d time.Duration) {
	e := event{VideoID: videoID, Title: title, Source: source, Nick: c.nick, SteamID: c.steamID,
		ServerIP: c.serverIP, PlayerIP: c.playerIP, DurationMs: d.Milliseconds()}
	if cached {
		m.cacheHits.Add(1)
		e.Type = "cache_hit"
	} else {
		m.conversions.Add(1)
		e.Type = "conversion"
		e.YtdlpMs = d.Milliseconds()
	}
	m.push(e)
}

func (m *Metrics) recordError(videoID, msg string, c caller, d time.Duration) {
	m.errors.Add(1)
	m.push(event{Type: "error", VideoID: videoID, Error: msg, Nick: c.nick, SteamID: c.steamID,
		ServerIP: c.serverIP, PlayerIP: c.playerIP, DurationMs: d.Milliseconds()})
}

// recent returns newest first.
func (m *Metrics) recent(limit int) []event {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.next
	if m.filled {
		n = maxEvents
	}
	if limit > n {
		limit = n
	}
	out := make([]event, 0, limit)
	for i := 1; i <= limit; i++ {
		out = append(out, m.ring[(m.next-i+maxEvents)%maxEvents])
	}
	return out
}

func (m *Metrics) avgConversionMs() any {
	evs := m.recent(maxEvents)
	var sum, n int64
	for _, e := range evs {
		if e.Type == "conversion" && e.YtdlpMs > 0 {
			sum += e.YtdlpMs
			n++
			if n == 50 {
				break
			}
		}
	}
	if n == 0 {
		return nil
	}
	return sum / n
}

func (m *Metrics) totals() map[string]int64 {
	return map[string]int64{
		"conversions":     m.conversions.Load(),
		"cacheHits":       m.cacheHits.Load(),
		"errors":          m.errors.Load(),
		"spotifyResolves": m.spotifyResolves.Load(),
		"apiCalls":        m.apiCalls.Load(),
	}
}

func (m *Metrics) system() map[string]any {
	load := [3]string{"0.00", "0.00", "0.00"}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		f := strings.Fields(string(b))
		for i := 0; i < 3 && i < len(f); i++ {
			load[i] = f[i]
		}
	}
	memTotal, memAvail := readMeminfo()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	up := int(time.Since(m.started).Seconds())
	return map[string]any{
		"uptime":        up,
		"processUptime": up,
		"cpu": map[string]any{
			"model": m.cpuModel, "cores": runtime.NumCPU(),
			"loadAvg1": load[0], "loadAvg5": load[1], "loadAvg15": load[2],
		},
		"memory": map[string]any{
			"totalMB":       memTotal >> 10,
			"usedMB":        (memTotal - memAvail) >> 10,
			"freeMB":        memAvail >> 10,
			"processRssMB":  readRSSKB() >> 10,
			"processHeapMB": ms.HeapAlloc >> 20,
		},
	}
}

func readCPUModel() string {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return runtime.GOARCH
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), ":"); ok && strings.TrimSpace(k) == "model name" {
			return strings.TrimSpace(v)
		}
	}
	return runtime.GOARCH
}

// readMeminfo returns total and available memory in kB.
func readMeminfo() (total, avail uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			avail = v
		}
	}
	return total, avail
}

func readRSSKB() uint64 {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "VmRSS:") {
			fields := strings.Fields(sc.Text())
			if len(fields) >= 2 {
				v, _ := strconv.ParseUint(fields[1], 10, 64)
				return v
			}
		}
	}
	return 0
}
