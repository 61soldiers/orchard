package apple

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// accountTTL caches the daemon's tokens. They are long-lived, so this only
// avoids hammering the account service on every catalog call.
const accountTTL = 5 * time.Minute

// ErrNotReady means the Apple session is not signed in yet.
var ErrNotReady = errors.New("apple session is not ready")

// Account is what the daemon's account service reports. Both tokens are
// secrets: never log them.
type Account struct {
	// StorefrontID is Apple's numeric storefront, e.g. "143441-1,31" for the US.
	StorefrontID string `json:"storefront_id"`
	// DevToken authenticates catalog requests as the Apple Music web player.
	DevToken string `json:"dev_token"`
	// MusicToken identifies the signed-in subscriber.
	MusicToken string `json:"music_token"`
}

// Account returns the daemon's tokens, cached for a few minutes.
func (m *Manager) Account(ctx context.Context) (Account, error) {
	m.mu.Lock()
	state := m.state
	m.mu.Unlock()
	if state != StateReady {
		return Account{}, ErrNotReady
	}

	m.accMu.Lock()
	defer m.accMu.Unlock()

	if time.Since(m.accAt) < accountTTL && m.acc.DevToken != "" {
		return m.acc, nil
	}

	addr := net.JoinHostPort(m.cfg.Host, strconv.Itoa(m.cfg.Ports.Account))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/", nil)
	if err != nil {
		return Account{}, err
	}

	resp, err := m.hc.Do(req)
	if err != nil {
		return Account{}, fmt.Errorf("read account info: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Account{}, fmt.Errorf("account service returned %s", resp.Status)
	}

	var acc Account
	if err := json.NewDecoder(resp.Body).Decode(&acc); err != nil {
		return Account{}, fmt.Errorf("decode account info: %w", err)
	}
	if acc.DevToken == "" || acc.MusicToken == "" {
		return Account{}, errors.New("account service returned no tokens")
	}

	m.acc, m.accAt = acc, time.Now()
	return acc, nil
}

// CatalogTokens adapts Account to the catalog client's token source.
func (m *Manager) CatalogTokens(ctx context.Context) (dev, mut string, err error) {
	acc, err := m.Account(ctx)
	if err != nil {
		return "", "", err
	}
	return acc.DevToken, acc.MusicToken, nil
}

// PlayActivityTokens adapts Account to the play-activity client's token
// source. That reporter needs the numeric storefront on top of the two tokens,
// which is why it is a separate source rather than reusing CatalogTokens.
func (m *Manager) PlayActivityTokens(ctx context.Context) (dev, mut, storefrontID string, err error) {
	acc, err := m.Account(ctx)
	if err != nil {
		return "", "", "", err
	}
	return acc.DevToken, acc.MusicToken, acc.StorefrontID, nil
}

// accountState is embedded in Manager; kept here so the cache lives with the
// code that uses it.
type accountState struct {
	accMu sync.Mutex
	acc   Account
	accAt time.Time
}
