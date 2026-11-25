package main

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
)

type bucket struct {
	T    int64 `json:"t"`
	Up   int64 `json:"u"`
	Down int64 `json:"d"`
}

// series keeps two resolutions: per-minute (last 6 h) and per-hour (last 35 days).
// Only buckets that saw traffic are stored, so idle keys cost nothing.
type series struct {
	Min []bucket `json:"min"`
	Hr  []bucket `json:"hr"`
}

const (
	keepMin = 360
	keepHr  = 24 * 35
)

func (s *series) add(now, up, down int64) {
	put := func(b []bucket, key int64, keep int) []bucket {
		if n := len(b); n > 0 && b[n-1].T == key {
			b[n-1].Up += up
			b[n-1].Down += down
			return b
		}
		b = append(b, bucket{key, up, down})
		if len(b) > keep {
			b = append([]bucket(nil), b[len(b)-keep:]...)
		}
		return b
	}
	s.Min = put(s.Min, now/60*60, keepMin)
	s.Hr = put(s.Hr, now/3600*3600, keepHr)
}

type History struct {
	mu    sync.Mutex
	path  string
	Total series             `json:"total"`
	Keys  map[string]*series `json:"keys"`
	dirty bool
}

func openHistory(path string) *History {
	h := &History{path: path, Keys: map[string]*series{}}
	if raw, err := os.ReadFile(path); err == nil {
		json.Unmarshal(raw, h)
		if h.Keys == nil {
			h.Keys = map[string]*series{}
		}
	}
	return h
}

func (h *History) add(id string, up, down, now int64) {
	if up <= 0 && down <= 0 {
		return
	}
	h.mu.Lock()
	s := h.Keys[id]
	if s == nil {
		s = &series{}
		h.Keys[id] = s
	}
	s.add(now, up, down)
	h.Total.add(now, up, down)
	h.dirty = true
	h.mu.Unlock()
}

func (h *History) forget(id string) {
	h.mu.Lock()
	delete(h.Keys, id) // the Total series keeps the deleted key's past traffic
	h.dirty = true
	h.mu.Unlock()
}

func (h *History) save() error {
	h.mu.Lock()
	if !h.dirty {
		h.mu.Unlock()
		return nil
	}
	j, _ := json.Marshal(h)
	h.dirty = false
	h.mu.Unlock()
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, j, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, h.path)
}

type rangeSpec struct {
	useMin bool
	step   int64
	count  int
}

var ranges = map[string]rangeSpec{
	"1h":  {true, 60, 60},
	"6h":  {true, 300, 72},
	"24h": {false, 3600, 24},
	"7d":  {false, 3 * 3600, 56},
	"30d": {false, 86400, 30},
}

type Point struct {
	T    int64 `json:"t"`
	Up   int64 `json:"up"`
	Down int64 `json:"down"`
}

func (s *series) window(rs rangeSpec, now int64) []Point {
	end := now/rs.step*rs.step + rs.step
	start := end - int64(rs.count)*rs.step
	pts := make([]Point, rs.count)
	for i := range pts {
		pts[i].T = start + int64(i)*rs.step
	}
	src := s.Hr
	if rs.useMin {
		src = s.Min
	}
	for _, b := range src {
		if b.T < start || b.T >= end {
			continue
		}
		i := (b.T - start) / rs.step
		pts[i].Up += b.Up
		pts[i].Down += b.Down
	}
	return pts
}

type KeyShare struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Up   int64  `json:"up"`
	Down int64  `json:"down"`
}

// query returns the chart points for "total" or a key id, and (for total) each live key's share.
func (h *History) query(id, rng string, now int64, names map[string]string) ([]Point, int64, []KeyShare, bool) {
	rs, ok := ranges[rng]
	if !ok {
		return nil, 0, nil, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if id == "total" {
		var shares []KeyShare
		for kid, s := range h.Keys {
			nm, live := names[kid]
			if !live {
				continue
			}
			var up, down int64
			for _, p := range s.window(rs, now) {
				up += p.Up
				down += p.Down
			}
			if up+down > 0 {
				shares = append(shares, KeyShare{kid, nm, up, down})
			}
		}
		sort.Slice(shares, func(i, j int) bool { return shares[i].Up+shares[i].Down > shares[j].Up+shares[j].Down })
		return h.Total.window(rs, now), rs.step, shares, true
	}
	s := h.Keys[id]
	if s == nil {
		s = &series{}
	}
	return s.window(rs, now), rs.step, nil, true
}
