package httpx

import (
	"testing"

	utls "github.com/refraction-networking/utls"
)

// TestBunClientHelloMatchesEmbeddedRuntime pins the re-captured fingerprint:
// the official embedded Bun does not offer X25519MLKEM768. Keeping the old
// 1.4.3 capture would make our ClientHello the only structural outlier (1473B
// against 251B) next to the CLI's own traffic.
func TestBunClientHelloMatchesEmbeddedRuntime(t *testing.T) {
	spec := bunClientHello()
	var curves, keyShares bool
	for _, extension := range spec.Extensions {
		switch value := extension.(type) {
		case *utls.SupportedCurvesExtension:
			curves = true
			for _, curve := range value.Curves {
				if curve == utls.X25519MLKEM768 {
					t.Fatal("ClientHello still offers X25519MLKEM768")
				}
			}
			if len(value.Curves) != 3 || value.Curves[0] != utls.X25519 {
				t.Fatalf("unexpected curves: %v", value.Curves)
			}
		case *utls.KeyShareExtension:
			keyShares = true
			for _, share := range value.KeyShares {
				if share.Group == utls.X25519MLKEM768 {
					t.Fatal("ClientHello still sends an MLKEM key share")
				}
			}
			if len(value.KeyShares) != 1 || value.KeyShares[0].Group != utls.X25519 {
				t.Fatalf("unexpected key shares: %v", value.KeyShares)
			}
		}
	}
	if !curves || !keyShares {
		t.Fatalf("missing curves=%v key shares=%v extensions", curves, keyShares)
	}
}
