package config

// Tier names a keyed upstream tier. It is also the type of the "prefer"
// setting, which picks the tier that is tried first when both serve a model.
type Tier string

const (
	// TierZen is the keyed Zen tier, served by upstream.zen, whose default is
	// the v1 /zen origin. Anonymous routes resolve to TierZen too, although
	// the keyless lane sends to its own origins (see UpstreamConfig.Anonymous).
	TierZen Tier = "zen"
	// TierGo is the keyed Go plan tier, served by upstream.go, whose default
	// is the v2 /zen/go origin.
	TierGo Tier = "go"
	// TierKilo is the keyless Kilo free-gateway lane. It is an alternative
	// upstream for an explicit model allowlist, never an OpenCode fallback and
	// never a value of Prefer: its quota and headers belong to a different
	// provider.
	TierKilo Tier = "kilo"
)
