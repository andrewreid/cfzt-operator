package naming

import (
	"crypto/sha256"
	"fmt"
)

const (
	// TokenSecretKey is the data key within the tunnel-token Secret.
	TokenSecretKey = "token"

	// Finalizer is registered on every CR the operator manages so
	// Cloudflare-side resources are cleaned up before the object is removed.
	Finalizer = "cfzt.reid.ee/finalizer"

	// failoverLeaseLabel is the fixed leftmost label of the DR failover
	// lease TXT record (D26). The leading underscore keeps the record in
	// the reserved-name space so it never collides with a published
	// hostname.
	failoverLeaseLabel = "_cfzt-lease"
)

// TokenSecretName returns the name of the Secret that holds the tunnel token.
func TokenSecretName(tunnelName string) string {
	return tunnelName + "-token"
}

// DaemonSetName returns the name of the cloudflared DaemonSet for a tunnel.
func DaemonSetName(tunnelName string) string {
	return "cloudflared-" + tunnelName
}

// AccessAppName returns the Cloudflare Access application name.
// displayName is preferred; metadataName is the fallback when displayName is empty.
func AccessAppName(displayName, metadataName string) string {
	base := displayName
	if base == "" {
		base = metadataName
	}
	return base + "-cfzt"
}

// FailoverLeaseTXTName returns the fully-qualified DNS name of the DR
// failover lease TXT record (D26):
//
//	_cfzt-lease.<hash8(groupID)>.<zone>
//
// The group ID is hashed (8 hex chars from SHA-256) rather than embedded
// verbatim so the lease record name stays a bounded, DNS-safe length and
// the user-chosen group string never leaks into public DNS. The same
// hash8 construction is used for generated tunnel names, so the two
// naming schemes stay visually consistent. zone is the longest-suffix
// matched zone for the exposure hostname; both clusters in a failover
// pair resolve the same zone and group and therefore the same record.
func FailoverLeaseTXTName(groupID, zone string) string {
	sum := sha256.Sum256([]byte(groupID))
	return fmt.Sprintf("%s.%x.%s", failoverLeaseLabel, sum[:4], zone)
}
