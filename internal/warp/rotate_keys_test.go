package warp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestRotateKeysKeepsDeviceIdentity proves that a key rotation PATCHes the
// existing device instead of deleting and re-registering it: the device ID and
// token survive, and no DELETE call is made.
func TestRotateKeysKeepsDeviceIdentity(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		if r.Method != http.MethodPatch {
			t.Errorf("unexpected method %s on %s: want PATCH (no delete/new)", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if body["key"] == "" {
			t.Error("rotation request missing new public key")
		}
		resp := deviceEnvelope{}
		resp.Result.ID = "device-1"
		resp.Result.Token = "token-1"
		resp.Result.Config.Interface.Addresses.V6 = "2001:db8::2/128"
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	r := newRegistrar()
	r.base = srv.URL
	anchor := registration{
		ID:            "device-1",
		Token:         "token-1",
		PrivateKey:    "old-private",
		PublicKey:     "old-public",
		AddressV4:     "10.0.0.2/32",
		AddressV6:     "2001:db8::1/128",
		PeerPublicKey: "peer",
		Endpoint:      "engage.cloudflareclient.com:2408",
	}
	got, err := r.rotateKeys(context.Background(), anchor)
	if err != nil {
		t.Fatalf("rotateKeys: %v", err)
	}
	if got.ID != "device-1" || got.Token != "token-1" {
		t.Fatalf("device identity changed: id=%q token=%q", got.ID, got.Token)
	}
	if got.PublicKey == anchor.PublicKey || got.PrivateKey == anchor.PrivateKey {
		t.Fatal("key material did not change")
	}
	if got.AddressV6 != "2001:db8::2/128" {
		t.Fatalf("new address not applied: %q", got.AddressV6)
	}
	if got.AddressV4 != anchor.AddressV4 {
		t.Fatalf("address omitted by PATCH was not kept: %q", got.AddressV4)
	}
	for _, m := range methods {
		if m == http.MethodDelete+" /reg/device-1" || m == http.MethodPost+" /reg" {
			t.Fatalf("destructive registration call present: %v", methods)
		}
	}
	if len(methods) != 1 {
		t.Fatalf("expected one PATCH, got %v", methods)
	}
}

// TestEarlyExitGuardSkipsAgeSweep proves an age-based retirement does not tear
// down an adapter while a request is in flight.
func TestEarlyExitGuardSkipsAgeSweep(t *testing.T) {
	gate := newLaneGate("")
	p := &Pool{
		ctx:      context.Background(),
		logger:   slogLogger{log: nil},
		size:     1,
		preload:  1,
		maxAge:   1, // anything is "aged"
		ageSweep: 0, // prevent the re-arm in sweepAged from scheduling
		active:   []*exit{},
		inflight: 1,
		laneGate: gate,
		reserved: map[string]bool{},
		cooling:  map[string]time.Time{},
		changed:  make(chan struct{}, 1),
	}
	p.sweepAged()
	if len(p.active) != 0 {
		t.Fatal("age sweep retired an exit while a request was in flight")
	}
}
