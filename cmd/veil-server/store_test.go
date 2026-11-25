package main

import (
	"testing"
	"time"

	"veil/proto"
)

func TestDeviceLimitAndStatus(t *testing.T) {
	a, err := newAccount(UserRec{ID: "x", Name: "t", PSK: newPSK(), Enabled: true, MaxDevices: 2})
	if err != nil {
		t.Fatal(err)
	}
	u1, ok := a.register("1.1.1.1", func() {})
	u1b, ok2 := a.register("1.1.1.1", func() {}) // same device, more streams: fine
	_, ok3 := a.register("2.2.2.2", func() {})
	_, ok4 := a.register("3.3.3.3", func() {}) // third distinct device: refused
	if !ok || !ok2 || !ok3 || ok4 {
		t.Fatalf("device limit wrong: %v %v %v %v", ok, ok2, ok3, ok4)
	}
	u1()
	u1b() // all streams of 1.1.1.1 gone -> slot frees up
	if _, ok := a.register("3.3.3.3", func() {}); !ok {
		t.Fatal("slot not freed")
	}
	now := time.Now().Unix()
	if a.status(now) != proto.StatusOK {
		t.Fatal("should be ok")
	}
	a.rec.ExpiresAt = now - 1
	a.apply()
	if a.status(now) != proto.StatusExpired {
		t.Fatal("should be expired")
	}
	a.rec.ExpiresAt, a.rec.DataCap = 0, 100
	a.apply()
	a.up.Store(100)
	if a.status(now) != proto.StatusQuota {
		t.Fatal("should be capped")
	}
}

func TestHistoryWindowAndReload(t *testing.T) {
	h := openHistory(t.TempDir() + "/h.json")
	now := time.Now().Unix()
	h.add("k1", 10, 100, now)
	h.add("k1", 5, 50, now)
	h.add("k2", 1, 1, now-7200)
	pts, step, shares, ok := h.query("total", "24h", now, map[string]string{"k1": "a", "k2": "b"})
	var up, down int64
	for _, p := range pts {
		up += p.Up
		down += p.Down
	}
	if !ok || step != 3600 || len(pts) != 24 || up != 16 || down != 151 || len(shares) != 2 || shares[0].ID != "k1" {
		t.Fatalf("bad window: %v %d %d %d %v", ok, step, up, down, shares)
	}
	if err := h.save(); err != nil {
		t.Fatal(err)
	}
	h2 := openHistory(h.path)
	pts2, _, _, _ := h2.query("k1", "1h", now, map[string]string{"k1": "a"})
	var d int64
	for _, p := range pts2 {
		d += p.Down
	}
	if d != 150 {
		t.Fatalf("reload lost data: %d", d)
	}
	h.forget("k1")
	if _, _, sh, _ := h.query("total", "24h", now, map[string]string{"k2": "b"}); len(sh) != 1 {
		t.Fatal("forgotten key still listed")
	}
}
