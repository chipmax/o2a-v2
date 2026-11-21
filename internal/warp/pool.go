package warp

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const DefaultPoolSize = 2

// burnPauseWindow bounds how long the pool stops minting new devices after
// several fresh exits were refused. A long pause (until UTC reset) makes the
// gateway unavailable for hours: retired exits leave the ring and no
// replacement is allowed, so readiness fails. A short window keeps retrying
// periodically without hammering Cloudflare's registration API.
const burnPauseWindow = 5 * time.Minute

// nextUTCReset is the next 00:00 UTC boundary. The free tier's per-/64 quota
// refills then, so a lifetime blacklist only has to hold a spent block until
// that moment rather than forever.
func nextUTCReset(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day()+1, 0, 0, 0, 0, time.UTC)
}

// egressCooldown keeps a public /64 (or IPv4 /32) that the upstream has
// rejected out of the pool for this long. The free tier scopes its quota to
// that block, so a new tunnel inside it would only repeat the same answer.
const egressCooldown = 30 * time.Minute

// refillRetryDelay spaces out a failed registration attempt. Successful
// registrations run back to back in the background.
const refillRetryDelay = 5 * time.Second

// A pool can only hold as many exits as Cloudflare hands out distinct egress
// blocks. When preload is larger than that supply, every extra registration is
// a duplicate; after a few in a row the pool stops trying for a while instead
// of hammering the registration API forever.
const (
	refillFailureThreshold = 3
	refillBackoffBase      = time.Minute
	refillBackoffMax       = 5 * time.Minute
)

// errDuplicateEgress reports a registration whose public egress block is
// already active or cooling down.
var errDuplicateEgress = errors.New("WARP egress is already active or cooling down")

// errSharedIPv4Egress reports a registration whose public egress is IPv4 while
// the pool requires a private /64 bucket. Cloudflare's IPv4 WARP egress is
// shared by every user, so keeping it would add an exit without adding quota.
var errSharedIPv4Egress = errors.New("WARP exit uses the shared IPv4 egress")

// errRegistrationRetryable marks a registration that should be tried again
// while the pool is still cold: a dual-stack device can lose its IPv6 probe
// to an unreachable endpoint even though the next device verifies fine.
var errRegistrationRetryable = errors.New("WARP registration is retryable")

// ipv6Required rejects an exit whose public egress cannot give this pool a
// separate free-tier bucket. An unset egress is rejected too: without a
// measured address there is no evidence it is a distinct bucket.
func ipv6Required(options Options, e *exit) bool {
	return options.RequireIPv6Egress && (e == nil || !e.egress.IsValid() || !e.egress.Is6())
}

// scaleDownDelay keeps exits created for a burst hot for a while after the
// burst, so a following burst reuses them instead of paying for new
// registrations.
const scaleDownDelay = 60 * time.Second

// DefaultRotateCooldown is the default spacing between two registrations
// caused by retiring an exit.
const DefaultRotateCooldown = 90 * time.Second

// ageSweepInterval is how often the pool looks for exits that reached MaxAge.
// A minute keeps an ageing rotation visible in the logs without polling often.
const ageSweepInterval = time.Minute

// ErrClosed is returned when a pool is used after Close. Callers use it to
// tell a retiring pool apart from a temporary capacity wait.
var ErrClosed = errors.New("WARP pool is closed")

// A laneGate paces registrations when the free lane refuses freshly registered
// exits. Cloudflare keeps handing out addresses in that state, but the upstream
// refuses each one within seconds, so minting device after device only spends
// the registration budget without ever landing on a usable egress. A process
// always gets one registration (every restart deserves a try); after a few
// refusals in a row the gate closes for a while.
type laneGate struct {
	mu       sync.Mutex
	refusals int
	until    time.Time
	// path keeps the gate across a restart: a process that just learned the
	// lane refuses fresh exits should not mint another device on boot.
	path string
}

// newLaneGate restores the pause saved by an earlier process. An expired pause
// keeps its refusal count for display but no longer blocks registration.
func newLaneGate(path string) *laneGate {
	gate := &laneGate{path: path}
	if path == "" {
		return gate
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return gate
	}
	var saved struct {
		Refusals int       `json:"refusals"`
		Until    time.Time `json:"until"`
	}
	if json.Unmarshal(data, &saved) != nil {
		return gate
	}
	gate.refusals = saved.Refusals
	gate.until = saved.Until
	return gate
}

func (g *laneGate) saveLocked() {
	if g == nil || g.path == "" {
		return
	}
	payload, err := json.Marshal(struct {
		Refusals int       `json:"refusals"`
		Until    time.Time `json:"until"`
	}{g.refusals, g.until})
	if err != nil {
		return
	}
	_ = os.WriteFile(g.path, payload, 0o600)
}

const (
	// laneGateFreeRefusals is how many new exits may be refused in a row
	// before registrations pause. A single burnt device is normal; three in a
	// row means the lane, not the registration, is the problem.
	laneGateFreeRefusals = 3
	laneGatePauseBase    = 5 * time.Minute
	laneGatePauseMax     = time.Hour
)

// state reports the pause in force, the consecutive refusal count and whether
// the gate is closed right now.
func (g *laneGate) state(now time.Time) (time.Time, int, bool) {
	if g == nil {
		return time.Time{}, 0, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	// A pause that expired a while ago must not keep its streak forever: the
	// next single refusal would otherwise jump straight back to a long pause.
	if !now.Before(g.until) && g.refusals > 0 && !g.until.IsZero() &&
		now.After(g.until.Add(laneGatePauseBase)) {
		g.refusals = 0
		g.until = time.Time{}
		g.saveLocked()
	}
	return g.until, g.refusals, now.Before(g.until)
}

// isGateRefusal distinguishes a request-shape/identity gate from a quota wall.
// Only the former means "fresh exits will be refused too"; a quota 429 is a
// property of the egress bucket and must not close registration.
func isGateRefusal(status int, body []byte) bool {
	if status != http.StatusForbidden && status != http.StatusTooManyRequests {
		return false
	}
	text := strings.ToLower(string(body))
	return strings.Contains(text, "freetiererror") ||
		strings.Contains(text, "free tier can only be used") ||
		strings.Contains(text, "free models aren't available from your current network")
}

// refused records one refused exit and returns the running count and the pause
// it imposes (0 while the free allowance lasts). The pause escalates with each
// additional refusal and never shrinks the one already in force.
func (g *laneGate) refused(now time.Time) (int, time.Duration) {
	if g == nil {
		return 0, 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refusals++
	if g.refusals <= laneGateFreeRefusals {
		g.saveLocked()
		return g.refusals, 0
	}
	pause := laneGatePauseBase << min(g.refusals-laneGateFreeRefusals-1, 4)
	if pause > laneGatePauseMax {
		pause = laneGatePauseMax
	}
	if until := now.Add(pause); until.After(g.until) {
		g.until = until
	}
	g.saveLocked()
	return g.refusals, pause
}

// admitted clears the gate after the lane accepted an exit again.
func (g *laneGate) admitted() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refusals = 0
	g.until = time.Time{}
	g.saveLocked()
}

// laneGatedError reports that the free lane refused too many fresh exits, so
// the pool is not registering another one yet. RetryAfter lets the refill loop
// pace itself instead of retrying into the same answer.
type laneGatedError struct {
	until    time.Time
	refusals int
}

func (e *laneGatedError) Error() string {
	return fmt.Sprintf("free lane refused %d fresh WARP exits; next registration after %s",
		e.refusals, e.until.UTC().Format(time.RFC3339))
}

func (e *laneGatedError) RetryAfter() time.Duration {
	return time.Until(e.until)
}

type logger interface {
	Debugf(string, ...interface{})
	Infof(string, ...interface{})
	Warnf(string, ...interface{})
	Errorf(string, ...interface{})
}

// slogLogger adapts the structured logger used by the gateway to the short
// diagnostic messages this package emits at its own lifecycle boundaries.
type slogLogger struct {
	log *slog.Logger
}

func (l slogLogger) logf(level slog.Level, format string, args ...interface{}) {
	if l.log == nil {
		return
	}
	l.log.Log(context.Background(), level, fmt.Sprintf(format, args...), "component", "warp")
}

func (l slogLogger) Debugf(format string, args ...interface{}) {
	l.logf(slog.LevelDebug, format, args...)
}

func (l slogLogger) Infof(format string, args ...interface{}) {
	l.logf(slog.LevelInfo, format, args...)
}

func (l slogLogger) Warnf(format string, args ...interface{}) {
	l.logf(slog.LevelWarn, format, args...)
}

func (l slogLogger) Errorf(format string, args ...interface{}) {
	l.logf(slog.LevelError, format, args...)
}

type Pool struct {
	ctx     context.Context
	cancel  context.CancelFunc
	logger  logger
	size    int
	preload int
	stack   Stack
	store   *registrationStore
	newExit func(context.Context) (*exit, error)

	maxAge         time.Duration
	rotateCooldown time.Duration
	ageSweep       time.Duration

	mu        sync.Mutex
	active    []*exit
	inflight  int
	waiting   int
	next      int
	reserved  map[string]bool
	cooling   map[string]time.Time
	changed   chan struct{}
	closed    bool
	refilling bool
	scaling   bool
	// lastRotate is when the previous voluntary (age) retirement was booked;
	// the next one waits until rotateCooldown has passed since then.
	lastRotate time.Time
	// refillFailures counts consecutive failed preload registrations;
	// refillPaused keeps every refill trigger quiet until it expires.
	refillFailures int
	refillPaused   time.Time
	// registering counts exits being registered right now (createExit is
	// serialized, so it is 0 or 1) and pending counts replacements queued by
	// penalizeFor. WaitForActive uses them to tell "a fresh exit is on its way"
	// from "the pool has nothing left to offer".
	registering int
	pending     int
	wait        sync.WaitGroup
	createMu    sync.Mutex
	// laneGate paces registrations once the free lane starts refusing every
	// freshly minted egress.
	laneGate *laneGate
	// burn holds the lifetime blacklist and the burned-streak pause. It has its
	// own type so the createExit closure can record verdicts before newPool has
	// assigned the Pool pointer (the first exit is built during construction).
	burn *burnState
}

// burnState tracks egress blocks the upstream proved exhausted. It is separate
// from Pool because the very first exit is created inside newPool, before the
// Pool value is available to the factory closure.
type burnState struct {
	mu                sync.Mutex
	spentLifetime     map[string]time.Time
	consecutiveBurned int
	burnedPausedUntil time.Time
}

func newBurnState() *burnState {
	return &burnState{spentLifetime: make(map[string]time.Time)}
}

type Lease struct {
	pool *Pool
	exit *exit
	once sync.Once
}

// Snapshot is a point-in-time view of a pool for health and admin reporting.
type Snapshot struct {
	Stack string `json:"stack"`
	Size  int    `json:"size"`
	// Preload is the number of exits kept hot even when the pool is idle:
	// half of Size (at least one), registered in the background after the
	// first exit so a burst does not wait for a registration.
	Preload int `json:"preload"`
	// Active is the number of exits currently in the ring. Standby counts the
	// idle part of it: exits not held by a lease and ready to replace a
	// penalized one immediately.
	Active  int `json:"active"`
	Standby int `json:"standby"`
	// Cooling is the number of egress prefixes the free lane refused recently.
	// It counts prefixes, not exits, so it can exceed Size while replacements
	// land on fresh blocks; CoolingPrefixes is the same number under its
	// explicit name for callers that display both.
	Cooling         int      `json:"cooling"`
	CoolingPrefixes int      `json:"cooling_prefixes"`
	Egress          []string `json:"egress,omitempty"`
	// LaneGated is true while the free lane refuses fresh exits often enough
	// that the pool stopped registering new ones; LaneUntil is when it tries
	// again and LaneRefusals counts the refusals in a row.
	LaneGated    bool      `json:"lane_gated"`
	LaneUntil    time.Time `json:"lane_until"`
	LaneRefusals int       `json:"lane_refusals"`
}

// Options configure a WARP pool.
type Options struct {
	// Size is the upper bound on exits. Beyond the preload the pool keeps as
	// many exits hot as the current concurrency needs and never exceeds Size.
	Size int
	// Preload is how many exits to keep warm while idle. Zero uses half of Size
	// (at least one); a positive value is capped at Size.
	Preload int
	// Stack selects the address family inside the tunnel. An IPv6 stack makes
	// the public egress an IPv6 address.
	Stack Stack
	// SourceIPv6, when valid, is the outer IPv6 prefix each exit binds its
	// WireGuard socket to. Every exit draws its own address from it, so a
	// replacement reaches Cloudflare from a new source and receives a new
	// public egress — the only way past an egress-scoped free usage limit.
	SourceIPv6 netip.Prefix
	// Sources, when non-empty, replaces SourceIPv6 with a set: every exit draws
	// its outer address from one prefix of the set, so several routed /64s — or
	// a single /48 from a tunnel broker — extend the number of independent
	// egress blocks the free tier meters separately.
	Sources []netip.Prefix
	// RequireIPv6Egress discards exits whose public egress is IPv4: that
	// address is shared by every Cloudflare user and its quota is usually
	// already spent. Useful with the dual stack on an IPv4-only host.
	RequireIPv6Egress bool
	// StatePath is where successful registrations are saved so a restart can
	// rebuild the pool without registering every exit again. An empty path
	// keeps registrations in memory only.
	StatePath string
	// LaneProbe, when its Model is set, is the one request a freshly built exit
	// must answer before the pool rotates traffic onto it. An exit that fails
	// it is closed and its Cloudflare device deleted, so the next attempt is a
	// new registration instead of a retry of the same spent egress.
	LaneProbe LaneProbe
	// MaxAge retires an idle exit once it has served this long, so a working
	// address is not kept for a whole day. Retirement happens between requests
	// and reuses the same replacement path as a refusal. Zero disables it.
	MaxAge time.Duration
	// RotateCooldown spaces out voluntary registrations: a replacement for an
	// exit retired on age waits until this much time has passed since the
	// previous voluntary retirement, so a burst of ageing exits does not mint
	// Cloudflare devices faster than its rate limit. Refused exits are replaced
	// at once, because the pool just lost capacity.
	RotateCooldown time.Duration
	// RotateKeys retires an exit by rotating its Cloudflare device key in place
	// rather than deleting the device. This avoids the 45 s adapter flapping and
	// ECONNRESET caused by the destructive delete/new cycle. Off by default so
	// existing deployments keep their exact behaviour.
	RotateKeys bool
}

// LaneProbe is the admission request for a new exit: a minimal free-lane
// completion. Auth is the full Authorization header value, empty on the
// anonymous lane.
type LaneProbe struct {
	URL   string
	Model string
	Auth  string
	// SystemPrompt, when set, is the leading system message of the admission
	// request. The gateway passes the same prompt it injects into real free-lane
	// traffic, because the gate judges that prompt: an env block without a
	// working directory is refused even from a healthy exit. Empty falls back to
	// a minimal gate-shaped block.
	SystemPrompt string
}

// NewWithOptions creates a WARP pool whose exits use the selected IP stack and,
// optionally, rotate their outer IPv6 source within a prefix.
func NewWithOptions(ctx context.Context, log *slog.Logger, opts Options) (*Pool, error) {
	normalized, err := ParseStack(string(opts.Stack))
	if err != nil {
		return nil, err
	}
	if opts.SourceIPv6.IsValid() && (!opts.SourceIPv6.Addr().Is6() || opts.SourceIPv6.Bits() > 64) {
		return nil, fmt.Errorf("WARP outer source must be an IPv6 prefix of /64 or shorter, got %s", opts.SourceIPv6)
	}
	for _, prefix := range opts.Sources {
		if !prefix.IsValid() || !prefix.Addr().Is6() || prefix.Bits() > 64 {
			return nil, fmt.Errorf("WARP outer sources must be IPv6 prefixes of /64 or shorter, got %s", prefix)
		}
	}
	size := opts.Size
	if size <= 0 {
		size = DefaultPoolSize
	}
	r := newRegistrar()
	store := newRegistrationStore(opts.StatePath, size)
	// drop deletes the Cloudflare device behind a retired exit. Cloudflare keeps
	// handing out the same egress block while the old devices still exist, so
	// deleting is what lets a replacement land on a different family.
	drop := func(id, token string) {
		dropCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := r.unregister(dropCtx, id, token); err != nil && log != nil {
			log.Warn("WARP registration not deleted", "component", "warp", "event", "registration_delete_failed", "id", id, "error", err)
		}
	}
	gatePath := ""
	if opts.StatePath != "" {
		gatePath = opts.StatePath + ".lane"
	}
	gate := newLaneGate(gatePath)
	// A fresh process always attempts one registration before the gate can stop
	// it: a saved registration may be gone and the lane may have recovered.
	var registrations atomic.Int32
	// maxRegistrationsPerMinute caps device minting. Cloudflare rate-limits
	// registration per network, and a burst of replacements would otherwise
	// spend the whole budget without producing a usable exit.
	const maxRegistrationsPerMinute = 6
	var regMu sync.Mutex
	var regTimes []time.Time
	throttleRegistration := func(ctx context.Context) error {
		for {
			regMu.Lock()
			now := time.Now()
			kept := regTimes[:0]
			for _, at := range regTimes {
				if now.Sub(at) < time.Minute {
					kept = append(kept, at)
				}
			}
			regTimes = kept
			if len(regTimes) < maxRegistrationsPerMinute {
				regTimes = append(regTimes, now)
				regMu.Unlock()
				return nil
			}
			wait := time.Minute - now.Sub(regTimes[0])
			regMu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
	}
	// The burn state exists before the first exit is built so the factory
	// closure can record verdicts during newPool's synchronous first exit.
	burn := newBurnState()
	pool, err := newPool(ctx, slogLogger{log: log}, store, burn, size, opts.Preload, opts.MaxAge, opts.RotateCooldown, gate, func(ctx context.Context) (*exit, error) {
		if err := throttleRegistration(ctx); err != nil {
			return nil, err
		}
		// Prefer a registration saved by an earlier process: it is ready to
		// use, so the pool stays warm across restarts. A saved registration
		// that no longer builds a working exit is dropped.
		for {
			reg, ok := store.pop()
			if !ok {
				break
			}
			source, err := opts.sampleSource()
			if err != nil {
				store.remember(reg)
				return nil, err
			}
			// Bound every probe: a dead registration should not eat the whole
			// startup budget before the next candidate is tried.
			probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
			reused, err := newExitWithStack(probeCtx, reg, normalized, source)
			cancel()
			if err == nil {
				reused.bindRegistration(reg, drop)
				// A saved registration can be perfectly healthy as a tunnel and
				// still be refused by the free lane: that is a spent device, not
				// a broken one, so it is deleted rather than kept for the next
				// restart.
				if admitErr := reused.admit(ctx, opts.LaneProbe); admitErr != nil {
					if errors.Is(admitErr, errLaneBurnt) {
						reused.markBurned()
					}
					reused.close()
					if ctx.Err() != nil {
						store.remember(reg)
						return nil, admitErr
					}
					slogLogger{log: log}.Warnf("saved WARP registration %s refused by the free lane (%v); registering a fresh exit", reg.ID, admitErr)
					continue
				}
				if ipv6Required(opts, reused) {
					reused.close()
					slogLogger{log: log}.Warnf("Discarding saved WARP exit %s: %v", reused.id, errSharedIPv4Egress)
					continue
				}
				gate.admitted()
				store.remember(reg)
				return reused, nil
			}
			// A probe cut short by the startup deadline says nothing about the
			// registration: keep it for the next process instead of spending a
			// fresh Cloudflare registration on every boot.
			if ctx.Err() != nil {
				store.remember(reg)
				return nil, err
			}
			slogLogger{log: log}.Warnf("saved WARP registration %s unusable (%v); registering a fresh exit", reg.ID, err)
			drop(reg.ID, reg.Token)
		}
		// A saved registration is always allowed (it costs no Cloudflare
		// device), but once the lane has refused several fresh ones in a row,
		// minting another is pointless until the pause it asked for ends.
		if registrations.Load() > 0 {
			if until, refusals, gated := gate.state(time.Now()); gated {
				return nil, &laneGatedError{until: until, refusals: refusals}
			}
			// Two confirmed new public /64s that still hit the free wall mean the
			// allowance is not following the address. Park new registrations until
			// the UTC reset instead of churning Cloudflare devices every rotation.
			burn.mu.Lock()
			pausedUntil := burn.burnedPausedUntil
			burnCount := burn.consecutiveBurned
			if !pausedUntil.After(time.Now()) && burnCount > 0 {
				// A pause window ended: give the next registration a clean slate
				// instead of re-pausing immediately on the first refusal.
				burn.consecutiveBurned = 0
			}
			burn.mu.Unlock()
			if pausedUntil.After(time.Now()) {
				return nil, &laneGatedError{until: pausedUntil, refusals: burnCount}
			}
		}
		var reg registration
		var err error
		rotated := false
		// With RotateKeys enabled, reuse an existing device and give it a fresh
		// WireGuard key instead of minting a new one. This is the non-destructive
		// counterpart to warp-cli's `tunnel rotate-keys`: the device ID stays,
		// so Cloudflare does not flap the adapter for ~45 s and in-flight
		// streams are not reset. Fall back to a normal registration if no anchor
		// exists or Cloudflare refuses the rotation.
		if opts.RotateKeys {
			if anchor, ok := store.anyKnown(); ok {
				reg, err = r.rotateKeys(ctx, anchor)
				rotated = err == nil
				if err != nil && log != nil {
					log.Warn("WARP key rotation failed; registering a fresh device", "component", "warp", "event", "rotate_keys_failed", "id", anchor.ID, "error", err)
				}
			}
		}
		if !rotated {
			reg, err = r.register(ctx)
		}
		if err != nil {
			return nil, err
		}
		registrations.Add(1)
		source, err := opts.sampleSource()
		if err != nil {
			drop(reg.ID, reg.Token)
			return nil, err
		}
		fresh, err := newExitWithStack(ctx, reg, normalized, source)
		if err != nil {
			drop(reg.ID, reg.Token)
			if opts.RequireIPv6Egress {
				return nil, fmt.Errorf("%w: %v", errRegistrationRetryable, err)
			}
			return nil, err
		}
		fresh.bindRegistration(reg, drop)
		// A burnt registration is not saved: its device is deleted when the exit
		// goes, so the next process would only spend a probe and a delete on it.
		keep := true
		if err := fresh.admit(ctx, opts.LaneProbe); err != nil {
			if errors.Is(err, errLaneBurnt) {
				keep = false
				// The tunnel works but the free lane refused the probe. The exit
				// is marked spent so its device is deleted when it goes.
				fresh.markBurned()
				// Honest rotation: only count a burn when the PUBLIC /64 really
				// changed. A device that keeps its old public block after a hop
				// must not advance the streak or it would park the pool early.
				key := fresh.egressKey()
				burn.mu.Lock()
				if key == "" || !burn.spentLifetime[key].After(time.Now()) {
					if key != "" {
						burn.spentLifetime[key] = nextUTCReset(time.Now())
					}
					burn.consecutiveBurned++
					if burn.consecutiveBurned >= 2 {
						burn.burnedPausedUntil = time.Now().Add(burnPauseWindow)
						slogLogger{log: log}.Warnf("OpenCode WARP: %d fresh /64s were refused; pausing registrations for %s", burn.consecutiveBurned, burnPauseWindow)
					}
				}
				burn.mu.Unlock()
				slogLogger{log: log}.Warnf("WARP exit %s admitted unverified: %v", fresh.id, err)
			} else {
				fresh.close()
				return nil, err
			}
		} else {
			gate.admitted()
			// A fresh exit that passed the free-lane probe proves quota follows
			// the new address; clear the burn streak so one bad block does not
			// poison later rotations.
			burn.mu.Lock()
			burn.consecutiveBurned = 0
			burn.mu.Unlock()
		}
		if ipv6Required(opts, fresh) {
			fresh.close()
			return nil, errSharedIPv4Egress
		}
		if keep {
			store.remember(reg)
		}
		return fresh, nil
	})
	if err != nil {
		return nil, err
	}
	pool.burn = burn
	pool.stack = normalized
	return pool, nil
}

// sampleSource draws the outer source address for one exit.
func (o Options) sampleSource() (netip.Addr, error) {
	prefixes := o.Sources
	if len(prefixes) == 0 {
		if !o.SourceIPv6.IsValid() {
			return netip.Addr{}, nil
		}
		prefixes = []netip.Prefix{o.SourceIPv6}
	}
	if len(prefixes) == 1 {
		return randomSource(prefixes[0])
	}
	var pick [1]byte
	if _, err := rand.Read(pick[:]); err != nil {
		return netip.Addr{}, fmt.Errorf("draw WARP outer source: %w", err)
	}
	return randomSource(prefixes[int(pick[0])%len(prefixes)])
}

// randomSource draws one host address from prefix. The bits above the prefix
// length are kept, every bit below is random, so two exits practically never
// share an outer source.
func randomSource(prefix netip.Prefix) (netip.Addr, error) {
	address := prefix.Masked().Addr().As16()
	var noise [16]byte
	if _, err := rand.Read(noise[:]); err != nil {
		return netip.Addr{}, fmt.Errorf("draw WARP outer source: %w", err)
	}
	keep := prefix.Bits()
	for index := range address {
		switch bits := keep - index*8; {
		case bits >= 8:
		case bits <= 0:
			address[index] = noise[index]
		default:
			mask := byte(0xFF) << (8 - bits)
			address[index] = address[index]&mask | noise[index]&^mask
		}
	}
	return netip.AddrFrom16(address), nil
}

func newPool(ctx context.Context, log logger, store *registrationStore, burn *burnState, size, preload int, maxAge, rotateCooldown time.Duration, gate *laneGate, create func(context.Context) (*exit, error)) (*Pool, error) {
	if burn == nil {
		burn = newBurnState()
	}
	if size <= 0 {
		size = DefaultPoolSize
	}
	// Keep exits warm so a burst finds ready exits instead of paying for
	// registrations while requests queue. The first exit is synchronous and
	// gates readiness; the rest register in the background. An explicit
	// preload from config lets an IPv4-only host warm more /64 exits without
	// waiting for rotations to trigger replacements.
	if preload <= 0 {
		preload = size / 2
	}
	if preload < 1 {
		preload = 1
	}
	if preload > size {
		preload = size
	}
	// The caller's context bounds the synchronous first registration, not the
	// pool: it is typically an initialization deadline that expires moments
	// after NewWithOptions returns. Deriving the pool lifetime from it would
	// cancel every later refill and replacement with "context canceled",
	// leaving the pool stuck on its first egress forever.
	poolCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	p := &Pool{
		ctx:            poolCtx,
		cancel:         cancel,
		logger:         log,
		store:          store,
		size:           size,
		preload:        preload,
		maxAge:         maxAge,
		rotateCooldown: rotateCooldown,
		ageSweep:       ageSweepFor(maxAge),
		laneGate:       gate,
		newExit:        create,
		reserved:       make(map[string]bool),
		cooling:        make(map[string]time.Time),
		burn:           burn,
		changed:        make(chan struct{}),
	}
	// Do not seed the cooling map from the persisted drain markers: after a
	// busy day every saved /64 can be marked, and refusing them all leaves the
	// pool without a first exit (health 503). A drained address is attempted
	// once more and, if the upstream still refuses it, the runtime lifetime
	// blacklist records it again.
	// The first registration gates readiness. Cloudflare can answer it with a
	// rate limit even when this process has just started (the limit follows the
	// source address), so wait out that window inside the caller's deadline
	// instead of failing startup over a pause that is about to end. Any other
	// failure still fails closed.
	first, err := p.createExit(ctx)
	sharedRetries, retryable := 0, 0
	for err != nil {
		if errors.Is(err, errRegistrationRetryable) && retryable < 6 {
			retryable++
			if log != nil {
				log.Warnf("OpenCode WARP first exit failed verification; registering another (%d/6): %v", retryable, err)
			}
			timer := time.NewTimer(5 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				cancel()
				return nil, ctx.Err()
			case <-timer.C:
			}
			first, err = p.createExit(ctx)
			continue
		}
		// A dual-stack registration can legitimately land on Cloudflare's
		// shared IPv4 egress. When the pool requires a private /64, retry a
		// few times instead of failing startup over one unlucky device.
		if errors.Is(err, errSharedIPv4Egress) && sharedRetries < 6 {
			sharedRetries++
			if log != nil {
				log.Warnf("OpenCode WARP first exit used the shared IPv4 egress; registering another (%d/6)", sharedRetries)
			}
			timer := time.NewTimer(5 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				cancel()
				return nil, ctx.Err()
			case <-timer.C:
			}
			first, err = p.createExit(ctx)
			continue
		}
		// Every known egress can be drained or cooling at once — for example
		// after a busy day, when the lifetime blacklist holds every saved /64.
		// That is a free-lane supply problem, not a broken configuration: keep
		// the gateway serving keyed traffic and let the refill loop look for a
		// fresh exit when the quota window resets.
		if errors.Is(err, errDuplicateEgress) || errors.Is(err, errLaneBurnt) {
			if log != nil {
				log.Warnf("OpenCode WARP first exit unavailable (%v); starting with an empty pool and retrying in the background", err)
			}
			first = nil
			err = nil
			break
		}
		pause := registrationPause(err)
		if pause <= 0 {
			cancel()
			return nil, err
		}
		if log != nil {
			log.Infof("OpenCode WARP first registration paused for %s", pause.Round(time.Second))
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			cancel()
			return nil, ctx.Err()
		case <-timer.C:
		}
		first, err = p.createExit(ctx)
	}
	if first != nil {
		p.active = []*exit{first}
		if log != nil {
			log.Infof("OpenCode WARP/%s pool ready (max %d exits, preloading %d)", first.stack, size, preload)
		}
	}
	p.startRefill()
	p.startAgeSweep()
	return p, nil
}

// ageSweepFor picks how often the pool looks for exits that reached maxAge:
// never for an unlimited age, otherwise at most a minute but early enough that
// an exit does not overshoot its limit by more than half of it.
func ageSweepFor(maxAge time.Duration) time.Duration {
	if maxAge <= 0 {
		return 0
	}
	interval := ageSweepInterval
	if half := maxAge / 2; half < interval {
		interval = half
		if interval < time.Second {
			interval = time.Second
		}
	}
	return interval
}

// startAgeSweep schedules the first look for exits that reached MaxAge. The
// sweep runs under the pool's lifetime, so Close stops it like any other timer.
func (p *Pool) startAgeSweep() {
	if p.ageSweep <= 0 {
		return
	}
	p.after(p.ageSweep, p.sweepAged)
}

// sweepAged retires one idle exit that has served MaxAge. It takes one exit per
// pass so the replacement registration — and its cooldown — is spread out, and
// it only takes idle exits, so an ageing rotation never interrupts a request.
func (p *Pool) sweepAged() {
	if p.maxAge <= 0 {
		return
	}
	// Retiring a working address only makes sense if a replacement can be
	// registered: while the lane refuses fresh exits, an aged rotation would
	// trade a usable egress for nothing.
	if _, _, gated := p.laneGate.state(time.Now()); gated {
		p.after(p.ageSweep, p.sweepAged)
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	// Early-exit guard: an age-based retirement is voluntary, so never tear
	// down an adapter while a request is in flight. Doing so is what produces
	// the 45 s flapping / ECONNRESET reported by warp-cli based rotators. A
	// quota-driven replacement still runs immediately; only the idle sweep
	// waits for a quiet moment.
	if p.inflight > 0 {
		p.mu.Unlock()
		p.after(p.ageSweep, p.sweepAged)
		return
	}
	now := time.Now()
	var aged *exit
	for _, candidate := range p.active {
		if candidate.age(now) >= p.maxAge && candidate.idle() {
			aged = candidate
			break
		}
	}
	p.mu.Unlock()
	if aged != nil {
		// The address is healthy, but the device is deleted with it: a fresh
		// registration is what yields a different egress /64.
		aged.markBurned()
		p.penalizeAfter(aged, "max age reached", 0, p.nextRotationDelay())
	}
	p.after(p.ageSweep, p.sweepAged)
}

// nextRotationDelay reports how long a voluntary rotation must wait so two
// registrations are at least rotateCooldown apart, and books that slot.
func (p *Pool) nextRotationDelay() time.Duration {
	if p.rotateCooldown <= 0 {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delay := p.rotateCooldown - time.Since(p.lastRotate)
	if delay < 0 {
		delay = 0
	}
	p.lastRotate = time.Now().Add(delay)
	return delay
}

// desiredLocked is the number of exits the pool should keep hot: one per
// in-flight lease, one for every request waiting for an exit, plus a spare, so
// a 429 can switch to a ready exit instead of waiting for a registration. The
// preload floor keeps half the ceiling registered while the pool is idle.
// Size is the upper bound; at idle a pool of two or more keeps one spare.
func (p *Pool) desiredLocked() int {
	want := p.inflight + p.waiting + 1
	if want < p.preload {
		want = p.preload
	}
	if want < 2 {
		want = 2
	}
	if want > p.size {
		want = p.size
	}
	return want
}

func (p *Pool) Acquire(ctx context.Context) (*Lease, error) {
	return p.AcquirePreferred(ctx, "")
}

// AcquirePreferred is Acquire with a hint: when preferred names an egress key
// (/64) that is still active, its exit is taken first so a session keeps the
// same quota scope and prompt cache. An empty or unknown hint falls back to the
// normal round-robin.
func (p *Pool) AcquirePreferred(ctx context.Context, preferred string) (*Lease, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, ErrClosed
		}
		if preferred != "" {
			for _, active := range p.active {
				if active.egressKey() != preferred {
					continue
				}
				if active.addRef() {
					p.inflight++
					p.mu.Unlock()
					return &Lease{pool: p, exit: active}, nil
				}
			}
		}
		for attempts := 0; attempts < len(p.active); attempts++ {
			index := p.next % len(p.active)
			p.next = (p.next + 1) % len(p.active)
			active := p.active[index]
			if active.addRef() {
				p.inflight++
				p.mu.Unlock()
				// Demand just rose: grow the ring toward it in the background.
				p.startRefill()
				return &Lease{pool: p, exit: active}, nil
			}
		}
		p.waiting++
		changed := p.changed
		needRefill := len(p.active) < p.desiredLocked()
		p.mu.Unlock()
		if needRefill {
			// Queued demand is demand too: start growing before we block.
			p.startRefill()
		}
		var err error
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-p.ctx.Done():
			err = ErrClosed
		case <-changed:
		}
		p.mu.Lock()
		p.waiting--
		p.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
}

func (p *Pool) Report(lease *Lease, status int, body []byte) {
	p.ReportWithRetryAfter(lease, status, body, 0)
}

// ReportWithRetryAfter is Report with the upstream's Retry-After hint. A hint
// longer than egressCooldown keeps the rejected egress cooling for that long.
func (p *Pool) ReportWithRetryAfter(lease *Lease, status int, body []byte, retryAfter time.Duration) {
	if lease == nil || lease.exit == nil {
		return
	}
	rotate, reason := lease.exit.observe(status, body)
	if !rotate {
		// A 2xx on real traffic is the only proof that this host is still
		// admitted, so it clears the refusal streak. The admission probe is not
		// used for this: it has refused exits that then served a 200.
		if status/100 == 2 {
			p.laneGate.admitted()
		}
		return
	}
	// While the lane refuses fresh exits, replacing this one would only trade a
	// working tunnel for another refusal: keep it so the client sees the
	// upstream's real 429 (with its Retry-After) instead of a minted device.
	if status == http.StatusTooManyRequests || isGateRefusal(status, body) {
		_, _, gated := p.laneGate.state(time.Now())
		if gated {
			if p.logger != nil {
				if until, _, _ := p.laneGate.state(time.Now()); !until.IsZero() {
					p.logger.Warnf("free lane is refusing fresh exits; keeping %s instead of rotating (registrations paused until %s)",
						lease.exit.id, until.UTC().Format(time.RFC3339))
				}
			}
			return
		}
		// Only a shape/identity gate says fresh exits will fail too. A quota
		// 429 retires this bucket but must not close the whole lane.
		if isGateRefusal(status, body) {
			refusals, pause := p.laneGate.refused(time.Now())
			if pause > 0 && p.logger != nil {
				p.logger.Warnf("free lane refused %d requests in a row across fresh exits; keeping exits and pausing registrations for %s", refusals, pause)
			}
		}
	}
	p.penalizeFor(lease.exit, reason, max(retryAfter, egressCooldown))
}

// ExitState is one WARP exit's live state for the quota board: its public
// egress, whether it is carrying a request, and when a cooling one reopens. A
// cooling exit is reported by egress key alone because its tunnel is gone.
type ExitState struct {
	ID           string    `json:"id,omitempty"`
	Egress       string    `json:"egress,omitempty"`
	EgressKey    string    `json:"egress_key,omitempty"`
	Source       string    `json:"source,omitempty"`
	InFlight     int       `json:"in_flight"`
	Cooling      bool      `json:"cooling,omitempty"`
	CoolingUntil time.Time `json:"cooling_until,omitempty"`
}

// States reports every active and cooling exit. It never exposes keys or
// device tokens.
func (p *Pool) States() []ExitState {
	if p == nil {
		return nil
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]ExitState, 0, len(p.active)+len(p.cooling))
	for _, active := range p.active {
		state := ExitState{ID: active.id, Egress: active.egress.String(), EgressKey: active.egressKey()}
		if active.source.IsValid() {
			state.Source = active.source.String()
		}
		active.mu.Lock()
		state.InFlight = active.refs
		active.mu.Unlock()
		out = append(out, state)
	}
	for key, until := range p.cooling {
		if until.After(now) {
			out = append(out, ExitState{EgressKey: key, Cooling: true, CoolingUntil: until})
		}
	}
	return out
}

// Retire removes every active exit on the given /64 from rotation and keeps the
// egress cooling until the free-tier reset. The exit is replaced in the
// background, so the pool keeps its size; only that prefix is dealt out again
// as a wasted attempt. It returns how many exits matched.
func (p *Pool) Retire(prefix string, until time.Time) int {
	cooldown := time.Until(until)
	if cooldown < time.Minute {
		cooldown = time.Minute
	}
	return p.penalizePrefix(prefix, "manual retire", cooldown)
}

// Rotate replaces every active exit on the given /64 with a fresh registration
// while keeping the old prefix cooling briefly, so the replacement cannot come
// back on the same egress.
func (p *Pool) Rotate(prefix string) int {
	return p.penalizePrefix(prefix, "manual rotate", time.Second)
}

func (p *Pool) penalizePrefix(prefix, reason string, cooldown time.Duration) int {
	if p == nil || prefix == "" {
		return 0
	}
	if parsed, err := netip.ParsePrefix(prefix); err == nil {
		prefix = parsed.Masked().String()
	}
	p.mu.Lock()
	matched := make([]*exit, 0, len(p.active))
	for _, active := range p.active {
		if active.egressKey() == prefix {
			matched = append(matched, active)
		}
	}
	p.mu.Unlock()
	for _, active := range matched {
		p.penalizeFor(active, reason, cooldown)
	}
	return len(matched)
}

// ReportQuotaSpent retires an exit whose egress answered 2xx with the free
// tier's quota error document. That body means the /64 has spent its bucket for
// the window, so the exit is dealt out again only as a wasted attempt; keeping
// it out of rotation (and out of registration reuse through cooling) until the
// next reset is what stops the same spent subnet from being picked repeatedly.
func (p *Pool) ReportQuotaSpent(lease *Lease, cooldown time.Duration) {
	if lease == nil || lease.exit == nil {
		return
	}
	if cooldown <= 0 {
		cooldown = egressCooldown
	}
	// The /64 proved spent is blacklisted until the next UTC reset. Even if the
	// caller asked for a short cooldown, a quota verdict is terminal for this
	// block for the rest of the counting window.
	key := lease.exit.egressKey()
	until := nextUTCReset(time.Now())
	if key != "" {
		p.burn.mu.Lock()
		if current := p.burn.spentLifetime[key]; !current.After(until) {
			p.burn.spentLifetime[key] = until
		}
		p.burn.mu.Unlock()
		p.mu.Lock()
		if current := p.cooling[key]; !current.After(until) {
			p.cooling[key] = until
		}
		p.mu.Unlock()
	}
	p.penalizeFor(lease.exit, "free tier quota document", cooldown)
}

// BurnedPaused reports whether the pool has parked new registrations because
// fresh public /64s kept hitting the quota wall. The caller can surface this in
// health output instead of silently churning Cloudflare devices.
func (p *Pool) BurnedPaused(now time.Time) (until time.Time) {
	if p == nil {
		return time.Time{}
	}
	p.burn.mu.Lock()
	defer p.burn.mu.Unlock()
	if p.burn.burnedPausedUntil.After(now) {
		return p.burn.burnedPausedUntil
	}
	return time.Time{}
}

// HasUntriedActive reports whether an active, unpenalized exit sits on an
// egress key that is not in tried, so another attempt would leave from a
// different quota scope.
func (p *Pool) HasUntriedActive(tried map[string]bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hasUntriedActiveLocked(tried)
}

func (p *Pool) hasUntriedActiveLocked(tried map[string]bool) bool {
	for _, active := range p.active {
		if active.isPenalized() {
			continue
		}
		if key := active.egressKey(); key == "" || !tried[key] {
			return true
		}
	}
	return false
}

// WaitForActive blocks until an active, unpenalized exit sits on an egress that
// is not in tried, or limit expires. It only waits while a registration is
// already in flight: a retired exit is replaced in the background, and giving
// up the moment the last active exit is penalized turns a rotation that is
// about to finish into a client-visible 429. When nothing is being registered
// it reports the current state immediately, so a genuinely exhausted pool still
// answers fast.
func (p *Pool) WaitForActive(ctx context.Context, tried map[string]bool, limit time.Duration) bool {
	if p == nil {
		return false
	}
	timer := time.NewTimer(limit)
	defer timer.Stop()
	for {
		p.mu.Lock()
		available := p.hasUntriedActiveLocked(tried)
		incoming := p.registering + p.pending
		changed := p.changed
		closed := p.closed
		p.mu.Unlock()
		if available {
			return true
		}
		if closed || incoming == 0 {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-p.ctx.Done():
			return false
		case <-changed:
		case <-timer.C:
			return false
		}
	}
}

// Fail drops an exit that cannot carry traffic even though the upstream never
// produced an HTTP response, such as a broken WARP handshake or a stalled first
// token. Retrying the same user-space tunnel would only repeat the stall.
func (p *Pool) Fail(lease *Lease, reason string) {
	if lease == nil || lease.exit == nil || reason == "" {
		return
	}
	p.penalize(lease.exit, reason)
}

func (p *Pool) Size() int { return p.size }

// Snapshot reports the pool's current capacity and the public egress address
// of every active exit. It never exposes keys or device tokens.
func (p *Pool) Snapshot() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	// A cooling entry outlives its window only on paper: drop it here so the
	// reported count follows the cooldown instead of growing all day.
	for key, until := range p.cooling {
		if !until.After(now) {
			delete(p.cooling, key)
		}
	}
	snapshot := Snapshot{
		Stack: string(p.stack), Size: p.size, Preload: p.preload, Active: len(p.active),
		Cooling: len(p.cooling), CoolingPrefixes: len(p.cooling),
	}
	if until, refusals, gated := p.laneGate.state(time.Now()); refusals > 0 || gated {
		snapshot.LaneGated = gated
		snapshot.LaneUntil = until
		snapshot.LaneRefusals = refusals
	}
	for _, active := range p.active {
		if active.idle() {
			snapshot.Standby++
		}
		if active.egress.IsValid() {
			snapshot.Egress = append(snapshot.Egress, active.egress.String())
		}
	}
	return snapshot
}

// ResetLaneGate clears the refusal streak and any pause so an operator can ask
// the pool for a fresh registration immediately, then starts a refill.
func (p *Pool) ResetLaneGate() {
	if p == nil || p.laneGate == nil {
		return
	}
	p.laneGate.admitted()
	p.startRefill()
}

func (p *Pool) penalize(bad *exit, reason string) {
	p.penalizeFor(bad, reason, egressCooldown)
}

func (p *Pool) penalizeFor(bad *exit, reason string, cooldown time.Duration) {
	p.penalizeAfter(bad, reason, cooldown, 0)
}

// penalizeAfter removes bad from the ring and schedules its replacement. delay
// postpones only the registration: the exit is gone at once, so a refusal
// (delay 0) keeps the pool responsive while a voluntary rotation can be spaced
// out to avoid registering devices in a burst.
func (p *Pool) penalizeAfter(bad *exit, reason string, cooldown, delay time.Duration) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	index := p.activeIndexLocked(bad)
	if index < 0 {
		p.mu.Unlock()
		return
	}
	if !bad.markPenalized() {
		p.mu.Unlock()
		return
	}
	// Quota penalties are terminal for this device: a spent /64 does not get
	// better when a replacement registration fails. Transport failures keep
	// the existing restore-on-empty behaviour.
	bad.mu.Lock()
	bad.quotaPenalty = strings.Contains(strings.ToLower(reason), "429") ||
		strings.Contains(strings.ToLower(reason), "quota") ||
		strings.Contains(strings.ToLower(reason), "spent")
	bad.mu.Unlock()
	if key := bad.egressKey(); key != "" {
		until := time.Now().Add(cooldown)
		p.cooling[key] = until
		delete(p.reserved, key)
		if p.store != nil {
			p.store.markDrain(bad.id, key, reason, until)
		}
	}
	p.active = append(p.active[:index], p.active[index+1:]...)
	if p.next >= len(p.active) {
		p.next = 0
	}
	p.pending++
	p.notifyLocked()
	p.wait.Add(1)
	p.mu.Unlock()

	if p.logger != nil {
		p.logger.Warnf("Rotating OpenCode WARP egress %s (%s)", bad.id, reason)
	}
	// Re-register immediately: the retired exit is replaced in the background
	// and the retry draws whichever active exit is ready first.
	go func() {
		defer p.wait.Done()
		defer func() {
			p.mu.Lock()
			if p.pending > 0 {
				p.pending--
			}
			p.notifyLocked()
			p.mu.Unlock()
		}()
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-p.ctx.Done():
				return
			case <-timer.C:
			}
		}
		p.replace(bad, reason)
	}()
}

func (p *Pool) replace(old *exit, reason string) {
	replacement, err := p.createExit(p.ctx)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		if replacement != nil {
			p.releaseReservation(replacement)
			replacement.close()
		}
		old.retire()
		return
	}
	if err != nil {
		old.mu.Lock()
		quotaRetired := old.quotaPenalty
		old.mu.Unlock()
		restored := len(p.active) == 0 && !quotaRetired
		if restored {
			old.clearPenalty()
			if key := old.egressKey(); key != "" {
				p.reserved[key] = true
				delete(p.cooling, key)
			}
			p.active = append(p.active, old)
			p.notifyLocked()
		}
		p.mu.Unlock()
		if !restored {
			old.retire()
		}
		if p.logger != nil {
			if pause := registrationPause(err); pause > 0 {
				p.logger.Infof("OpenCode WARP replacement paused for %s", pause.Round(time.Second))
			} else {
				p.logger.Errorf("Cannot replace OpenCode WARP egress %s: %v", old.id, err)
			}
		}
		p.startRefill()
		return
	}
	if p.activeIndexLocked(old) >= 0 {
		p.mu.Unlock()
		p.releaseReservation(replacement)
		replacement.close()
		old.retire()
		return
	}
	placed := false
	if len(p.active) < p.size {
		p.active = append(p.active, replacement)
		placed = true
	}
	p.notifyLocked()
	p.mu.Unlock()
	if !placed {
		p.releaseReservation(replacement)
		replacement.close()
		old.retire()
		return
	}
	if p.logger != nil {
		p.logger.Infof("OpenCode WARP egress %s replaced by %s at %s (%s), active=%d", old.id, replacement.id, replacement.egress, reason, len(p.active))
	}
	old.retire()
	p.startRefill()
}

func (p *Pool) refill() {
	p.mu.Lock()
	if p.closed || p.refilling || time.Now().Before(p.refillPaused) || !p.needsRefillLocked() {
		p.mu.Unlock()
		return
	}
	p.refilling = true
	p.mu.Unlock()

	replacement, err := p.createExit(p.ctx)
	p.mu.Lock()
	p.refilling = false
	placed := false
	if err == nil && !p.closed && len(p.active) < p.size {
		p.active = append(p.active, replacement)
		replacement = nil
		placed = true
	}
	if err == nil {
		p.refillFailures = 0
		p.refillPaused = time.Time{}
	}
	if placed || p.closed {
		p.notifyLocked()
	}
	p.mu.Unlock()

	if replacement != nil {
		p.releaseReservation(replacement)
		replacement.close()
	}
	if err != nil && p.ctx.Err() == nil {
		delay := refillRetryDelay
		if pause := registrationPause(err); pause > 0 {
			// Cloudflare's rate limit is the pacing: wait out the window it
			// asked for instead of retrying into the same answer.
			delay = pause
			if delay < refillRetryDelay {
				delay = refillRetryDelay
			}
			if delay > maxRateLimitPause {
				delay = maxRateLimitPause
			}
			if p.logger != nil {
				p.logger.Infof("OpenCode WARP registration paused for %s", delay.Round(time.Second))
			}
		} else if errors.Is(err, errDuplicateEgress) {
			p.mu.Lock()
			p.refillFailures++
			failures := p.refillFailures
			p.mu.Unlock()
			if failures >= refillFailureThreshold {
				// The egress supply is smaller than the target. Back off
				// exponentially so a saturated colo is not registered into
				// forever.
				shift := failures - refillFailureThreshold
				if shift > 3 {
					shift = 3
				}
				delay = refillBackoffBase << shift
				if delay > refillBackoffMax {
					delay = refillBackoffMax
				}
				if p.logger != nil {
					p.logger.Infof("OpenCode WARP preload paused for %s after %d duplicate egresses", delay.Round(time.Second), failures)
				}
			}
		} else if p.logger != nil {
			p.logger.Warnf("Cannot pre-register OpenCode WARP egress: %v", err)
		}
		p.mu.Lock()
		if paused := time.Now().Add(delay); paused.After(p.refillPaused) {
			p.refillPaused = paused
		}
		p.mu.Unlock()
		p.after(delay, p.retryRefill)
		return
	}
	p.startRefill()
}

// retryRefill is the timer half of a failed refill: it clears the pause that
// kept ordinary triggers quiet and asks for another attempt. Going through
// startRefill (rather than calling refill directly) keeps the single-flight
// guard.
func (p *Pool) retryRefill() {
	p.mu.Lock()
	p.refillPaused = time.Time{}
	p.mu.Unlock()
	p.startRefill()
}

func (p *Pool) startRefill() {
	p.mu.Lock()
	if p.closed || p.refilling || time.Now().Before(p.refillPaused) || !p.needsRefillLocked() {
		p.mu.Unlock()
		return
	}
	p.wait.Add(1)
	p.mu.Unlock()
	go func() {
		defer p.wait.Done()
		p.refill()
	}()
}

// released records that a lease was returned and trims the ring back toward
// the demand of the moment. Freed exits stay hot for scaleDownDelay, so a
// burst that pauses does not immediately pay for re-registering them.
func (p *Pool) released() {
	p.mu.Lock()
	if p.inflight > 0 {
		p.inflight--
	}
	closed := p.closed
	wantRefill := !closed && len(p.active) < p.desiredLocked()
	started := p.scaling
	if !closed && !started && len(p.active) > p.desiredLocked() {
		p.scaling = true
		started = true
	}
	p.mu.Unlock()
	if closed {
		return
	}
	if wantRefill {
		p.startRefill()
	}
	if started {
		p.after(scaleDownDelay, p.scaleDown)
	}
}

// scaleDown retires every idle exit above the current demand. Busy surplus
// exits are left to their release, which arms another pass if the ring is
// still over target then.
func (p *Pool) scaleDown() {
	p.mu.Lock()
	p.scaling = false
	if p.closed {
		p.mu.Unlock()
		return
	}
	want := p.desiredLocked()
	var dropped []*exit
	if surplus := len(p.active) - want; surplus > 0 {
		kept := p.active[:0]
		for _, candidate := range p.active {
			if surplus > 0 && candidate.idle() {
				dropped = append(dropped, candidate)
				surplus--
				continue
			}
			kept = append(kept, candidate)
		}
		p.active = kept
	}
	if p.next >= len(p.active) {
		p.next = 0
	}
	p.mu.Unlock()
	for _, drop := range dropped {
		p.releaseReservation(drop)
		drop.retire()
		if p.logger != nil {
			p.logger.Infof("OpenCode WARP egress %s scaled down at %s", drop.id, drop.egress)
		}
	}
}

// after runs fn once delay has elapsed, unless the pool closes first. The wait
// group is incremented under the pool lock so it cannot race with Close's
// Wait: a timer scheduled after Close is not started at all, and one scheduled
// before it is counted before Close can begin waiting.
func (p *Pool) after(delay time.Duration, fn func()) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.wait.Add(1)
	p.mu.Unlock()
	go func() {
		defer p.wait.Done()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-p.ctx.Done():
		case <-timer.C:
			fn()
		}
	}()
}

func (p *Pool) createExit(ctx context.Context) (*exit, error) {
	p.createMu.Lock()
	defer p.createMu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		p.mu.Lock()
		closed := p.closed
		p.mu.Unlock()
		if closed {
			return nil, ErrClosed
		}
		p.mu.Lock()
		p.registering++
		p.mu.Unlock()
		// Snapshot the lifetime blacklist before the attempt: the admission
		// probe writes the candidate's own key into it on a 429, and that
		// marker must not reject the very exit that produced it.
		var spentBefore map[string]bool
		if p.burn != nil {
			p.burn.mu.Lock()
			spentBefore = make(map[string]bool, len(p.burn.spentLifetime))
			for k := range p.burn.spentLifetime {
				spentBefore[k] = true
			}
			p.burn.mu.Unlock()
		}
		candidate, err := p.newExit(ctx)
		p.mu.Lock()
		p.registering--
		p.mu.Unlock()
		if err != nil {
			// An exit whose free-lane allowance is already spent is not a
			// Cloudflare registration problem: retrying instantly only spends
			// another device on the same family, so pace the next attempt.
			if errors.Is(err, errLaneBurnt) {
				timer := time.NewTimer(5 * time.Second)
				select {
				case <-p.ctx.Done():
					timer.Stop()
					return nil, err
				case <-timer.C:
				}
			}
			return nil, err
		}
		key := candidate.egressKey()
		now := time.Now()
		p.mu.Lock()
		for subnet, until := range p.cooling {
			if !until.After(now) {
				delete(p.cooling, subnet)
			}
		}
		spent := false
		if p.burn != nil {
			p.burn.mu.Lock()
			for subnet, until := range p.burn.spentLifetime {
				if !until.After(now) {
					delete(p.burn.spentLifetime, subnet)
				}
			}
			spent = spentBefore[key] && p.burn.spentLifetime[key].After(now)
			p.burn.mu.Unlock()
		}
		// A block the upstream proved spent stays out until 00:00 UTC even if
		// the short cooling entry expired. This is the lifetime blacklist.
		usable := key != "" && !p.reserved[key] && !p.cooling[key].After(now) && !spent
		if usable {
			p.reserved[key] = true
		}
		p.mu.Unlock()
		if usable {
			if p.logger != nil {
				p.logger.Infof("OpenCode WARP/%s egress %s ready at %s", candidate.stack, candidate.id, candidate.egress)
			}
			return candidate, nil
		}
		lastErr = fmt.Errorf("%w: %s", errDuplicateEgress, key)
		if p.logger != nil {
			p.logger.Warnf("Discarding duplicate WARP exit %s: %v", candidate.id, lastErr)
		}
		candidate.close()
	}
	return nil, lastErr
}

func (p *Pool) releaseReservation(e *exit) {
	if e == nil {
		return
	}
	key := e.egressKey()
	if key == "" {
		return
	}
	p.mu.Lock()
	delete(p.reserved, key)
	p.mu.Unlock()
}

func (p *Pool) notifyLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *Pool) activeIndexLocked(target *exit) int {
	for index, candidate := range p.active {
		if candidate == target {
			return index
		}
	}
	return -1
}

func (p *Pool) needsRefillLocked() bool {
	return len(p.active) < p.desiredLocked()
}

func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	active := p.active
	p.active = nil
	p.notifyLocked()
	p.mu.Unlock()

	p.cancel()
	for _, item := range active {
		p.releaseReservation(item)
		item.retire()
	}
	p.wait.Wait()
}

func (l *Lease) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if l == nil || l.exit == nil {
		return nil, errors.New("WARP lease is closed")
	}
	return l.exit.dialContext(ctx, network, address)
}

func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.exit != nil {
			l.exit.release()
		}
		if l.pool != nil {
			l.pool.released()
		}
	})
}

func (l *Lease) EgressKey() string {
	if l == nil || l.exit == nil {
		return ""
	}
	return l.exit.egressKey()
}

// DialNetwork is the explicit TCP address family for this lease. Callers should
// pass it to DialContext instead of assuming IPv6.
func (l *Lease) DialNetwork() string {
	if l == nil || l.exit == nil {
		return ""
	}
	return l.exit.dialNetwork()
}

func (l *Lease) String() string {
	if l == nil || l.exit == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s/%s", l.exit.id, l.exit.egress)
}
