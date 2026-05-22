package cloudflare

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultAccountIDKey = "accountId"
	defaultAPITokenKey  = "apiToken"
)

// CredentialsRef identifies a Kubernetes Secret and the keys that hold
// Cloudflare credentials.
type CredentialsRef struct {
	Namespace    string
	Name         string
	AccountIDKey string
	APITokenKey  string
}

// Load reads Cloudflare credentials from the referenced Kubernetes Secret.
func Load(ctx context.Context, c client.Reader, ref CredentialsRef) (string, string, error) {
	var secret corev1.Secret
	key := types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}
	if err := c.Get(ctx, key, &secret); err != nil {
		return "", "", fmt.Errorf("credentials Secret %s/%s not readable: %w", key.Namespace, key.Name, err)
	}

	accountKey := ref.AccountIDKey
	if accountKey == "" {
		accountKey = defaultAccountIDKey
	}
	tokenKey := ref.APITokenKey
	if tokenKey == "" {
		tokenKey = defaultAPITokenKey
	}

	accountID := string(secret.Data[accountKey])
	if accountID == "" {
		return "", "", fmt.Errorf("credentials Secret %s/%s missing key %q", key.Namespace, key.Name, accountKey)
	}
	apiToken := string(secret.Data[tokenKey])
	if apiToken == "" {
		return "", "", fmt.Errorf("credentials Secret %s/%s missing key %q", key.Namespace, key.Name, tokenKey)
	}
	return accountID, apiToken, nil
}
