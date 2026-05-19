package naming

const (
	// TokenSecretKey is the data key within the tunnel-token Secret.
	TokenSecretKey = "token"

	// Finalizer is registered on every CR the operator manages so
	// Cloudflare-side resources are cleaned up before the object is removed.
	Finalizer = "cfzt.reid.ee/finalizer"
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
