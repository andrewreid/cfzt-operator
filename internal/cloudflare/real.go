package cloudflare

import (
	"context"
	"errors"
	"math/rand"
	"net/http"
	"time"

	cf "github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/option"
	"github.com/cloudflare/cloudflare-go/v4/zero_trust"
	"golang.org/x/time/rate"
)

const (
	defaultRateLimit = 4.0 // requests/second per API token
	defaultBurst     = 8
	maxRetries       = 5
)

// RealClient wraps the cloudflare-go/v4 SDK behind the Client interface.
// All SDK imports are confined to this file; controllers never see cloudflare-go.
type RealClient struct {
	api       *cf.Client
	accountID string
	limiter   *rate.Limiter
}

// New constructs a RealClient authenticated with apiToken for accountID.
func New(accountID, apiToken string) (*RealClient, error) {
	if accountID == "" {
		return nil, errors.New("cloudflare: accountID required")
	}
	if apiToken == "" {
		return nil, errors.New("cloudflare: apiToken required")
	}
	api := cf.NewClient(option.WithAPIToken(apiToken))
	return &RealClient{
		api:       api,
		accountID: accountID,
		limiter:   rate.NewLimiter(defaultRateLimit, defaultBurst),
	}, nil
}

func (c *RealClient) Tunnels() Tunnels {
	return &realTunnels{client: c}
}

// withRetry waits for the rate limiter then calls fn, retrying on 429 / 5xx
// with exponential backoff + jitter up to maxRetries attempts.
func (c *RealClient) withRetry(ctx context.Context, fn func() error) error {
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return err
		}
		err := fn()
		if err == nil {
			return nil
		}
		if attempt == maxRetries {
			return err
		}
		var apiErr *cf.Error
		if !errors.As(err, &apiErr) {
			return err // not an API error; no retry
		}
		code := apiErr.StatusCode
		if code != http.StatusTooManyRequests && (code < 500 || code > 599) {
			return err
		}
		// exponential backoff with jitter: base 500ms * 2^attempt ± 25%
		base := time.Duration(500<<uint(attempt)) * time.Millisecond
		jitter := time.Duration(rand.Int63n(int64(base) / 2))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(base + jitter - base/4):
		}
	}
	return nil // unreachable
}

// realTunnels implements the Tunnels sub-interface against the SDK.
type realTunnels struct {
	client *RealClient
}

func (t *realTunnels) Create(ctx context.Context, in CreateTunnelInput) (*Tunnel, error) {
	configSrc := zero_trust.TunnelCloudflaredNewParamsConfigSrc(in.ConfigSrc)
	var result *Tunnel
	err := t.client.withRetry(ctx, func() error {
		resp, err := t.client.api.ZeroTrust.Tunnels.Cloudflared.New(ctx,
			zero_trust.TunnelCloudflaredNewParams{
				AccountID: cf.F(t.client.accountID),
				Name:      cf.F(in.Name),
				ConfigSrc: cf.F(configSrc),
			},
		)
		if err != nil {
			return err
		}
		result = &Tunnel{ID: resp.ID, Name: resp.Name}
		return nil
	})
	return result, err
}

func (t *realTunnels) List(ctx context.Context, filter ListTunnelsFilter) ([]Tunnel, error) {
	params := zero_trust.TunnelCloudflaredListParams{
		AccountID: cf.F(t.client.accountID),
	}
	if filter.Name != "" {
		params.Name = cf.F(filter.Name)
	}

	var results []Tunnel
	err := t.client.withRetry(ctx, func() error {
		pager := t.client.api.ZeroTrust.Tunnels.Cloudflared.ListAutoPaging(ctx, params)
		results = results[:0]
		for pager.Next() {
			item := pager.Current()
			results = append(results, Tunnel{
				ID:   item.ID,
				Name: item.Name,
			})
		}
		return pager.Err()
	})
	return results, err
}

func (t *realTunnels) Get(ctx context.Context, id string) (*Tunnel, error) {
	var result *Tunnel
	err := t.client.withRetry(ctx, func() error {
		resp, err := t.client.api.ZeroTrust.Tunnels.Cloudflared.Get(ctx, id,
			zero_trust.TunnelCloudflaredGetParams{
				AccountID: cf.F(t.client.accountID),
			},
		)
		if err != nil {
			var apiErr *cf.Error
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
				return ErrNotFound
			}
			return err
		}
		result = &Tunnel{
			ID:   resp.ID,
			Name: resp.Name,
		}
		return nil
	})
	return result, err
}

func (t *realTunnels) Delete(ctx context.Context, id string) error {
	return t.client.withRetry(ctx, func() error {
		_, err := t.client.api.ZeroTrust.Tunnels.Cloudflared.Delete(ctx, id,
			zero_trust.TunnelCloudflaredDeleteParams{
				AccountID: cf.F(t.client.accountID),
			},
		)
		return err
	})
}

func (t *realTunnels) Token(ctx context.Context, id string) (string, error) {
	var tok string
	err := t.client.withRetry(ctx, func() error {
		resp, err := t.client.api.ZeroTrust.Tunnels.Cloudflared.Token.Get(ctx, id,
			zero_trust.TunnelCloudflaredTokenGetParams{
				AccountID: cf.F(t.client.accountID),
			},
		)
		if err != nil {
			return err
		}
		if resp != nil {
			tok = *resp
		}
		return nil
	})
	return tok, err
}
