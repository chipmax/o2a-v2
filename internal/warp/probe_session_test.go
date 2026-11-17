package warp

import (
	"encoding/hex"
	"regexp"
	"strconv"
	"testing"
	"time"
)

var probeSessionPattern = regexp.MustCompile(`^ses_([0-9a-f]{12})([0-9A-Za-z]{14})$`)

// sessionStamp decodes the 12 hex characters the way the gate does: the low 48
// bits of ~(unix_ms<<12|counter), i.e. only the low 36 bits of the millisecond
// clock survive. A value is plausible when that residue is near the current one.
func sessionStamp(id string) (int64, bool) {
	match := probeSessionPattern.FindStringSubmatch(id)
	if match == nil {
		return 0, false
	}
	raw, err := hex.DecodeString(match[1])
	if err != nil {
		return 0, false
	}
	var packed uint64
	for _, byteValue := range raw {
		packed = packed<<8 | uint64(byteValue)
	}
	return int64((^packed & 0xFFFFFFFFFFFF) >> 12), true
}

func TestProbeSessionEncodesTheCurrentTime(t *testing.T) {
	const wrap = int64(1) << 36
	now := time.Now().UnixMilli() & (wrap - 1)
	for i := 0; i < 32; i++ {
		id := probeSession()
		stamp, ok := sessionStamp(id)
		if !ok {
			t.Fatalf("probe session %q does not have the canonical shape", id)
		}
		delta := stamp - now
		if delta > wrap/2 {
			delta -= wrap
		}
		if delta < -wrap/2 {
			delta += wrap
		}
		if delta < -120000 || delta > 120000 {
			t.Fatalf("probe session %q decodes %d ms from now; the gate reads that as a foreign date", id, delta)
		}
	}
}

func TestProbeSessionIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 16; i++ {
		id := probeSession()
		if seen[id] {
			t.Fatalf("probe session %q was minted twice", id)
		}
		seen[id] = true
	}
}

func TestProbeSessionLowBitsMatchTheCounterWindow(t *testing.T) {
	id := probeSession()
	match := probeSessionPattern.FindStringSubmatch(id)
	if match == nil {
		t.Fatalf("probe session %q is malformed", id)
	}
	low, err := strconv.ParseUint(match[1][9:], 16, 16)
	if err != nil {
		t.Fatalf("low bits of %q are not hex: %v", id, err)
	}
	if low < 0xfc0 || low > 0xfff {
		t.Fatalf("probe session %q has low bits %#x outside the accepted counter window", id, low)
	}
}
