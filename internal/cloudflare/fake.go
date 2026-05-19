package cloudflare

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"sync"

	"github.com/google/uuid"
)

// FakeClient is an in-memory implementation of Client for unit tests.
// No global state; each instance is independent.
type FakeClient struct {
	mu      sync.Mutex
	tunnels map[string]*Tunnel
}

// NewFake returns a ready-to-use FakeClient.
func NewFake() *FakeClient {
	return &FakeClient{tunnels: make(map[string]*Tunnel)}
}

func (f *FakeClient) Tunnels() Tunnels {
	return &fakeTunnels{fc: f}
}

type fakeTunnels struct {
	fc *FakeClient
}

func (t *fakeTunnels) Create(_ context.Context, in CreateTunnelInput) (*Tunnel, error) {
	t.fc.mu.Lock()
	defer t.fc.mu.Unlock()
	id := uuid.New().String()
	tun := &Tunnel{ID: id, Name: in.Name}
	t.fc.tunnels[id] = tun
	copy := *tun
	return &copy, nil
}

func (t *fakeTunnels) List(_ context.Context, filter ListTunnelsFilter) ([]Tunnel, error) {
	t.fc.mu.Lock()
	defer t.fc.mu.Unlock()
	var out []Tunnel
	for _, tun := range t.fc.tunnels {
		if filter.Name != "" && tun.Name != filter.Name {
			continue
		}
		out = append(out, *tun)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (t *fakeTunnels) Get(_ context.Context, id string) (*Tunnel, error) {
	t.fc.mu.Lock()
	defer t.fc.mu.Unlock()
	tun, ok := t.fc.tunnels[id]
	if !ok {
		return nil, ErrNotFound
	}
	copy := *tun
	return &copy, nil
}

func (t *fakeTunnels) Delete(_ context.Context, id string) error {
	t.fc.mu.Lock()
	defer t.fc.mu.Unlock()
	if _, ok := t.fc.tunnels[id]; !ok {
		return ErrNotFound
	}
	delete(t.fc.tunnels, id)
	return nil
}

// Token returns a deterministic token derived from the tunnel ID so callers get
// the same value on repeated calls and different IDs produce different tokens.
func (t *fakeTunnels) Token(_ context.Context, id string) (string, error) {
	t.fc.mu.Lock()
	defer t.fc.mu.Unlock()
	if _, ok := t.fc.tunnels[id]; !ok {
		return "", ErrNotFound
	}
	sum := sha256.Sum256([]byte("fake-token:" + id))
	return fmt.Sprintf("%x", sum), nil
}
