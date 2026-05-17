package browser

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/go-rod/rod/lib/proto"
)

// credentialKey returns the TS-01 KV key for a domain's cookies.
func credentialKey(domain string) string {
	return fmt.Sprintf("browser:%s:cookies", domain)
}

const credentialNamespace = "credentials"

// GetCookies loads cookies for a domain from TS-01 KV store.
func (r *browserRuntime) GetCookies(ctx context.Context, domain string) (Cookie, error) {
	return r.loadCookiesFromKV(ctx, domain)
}

// SetCookies saves cookies for a domain to TS-01 KV store.
func (r *browserRuntime) SetCookies(ctx context.Context, cookies Cookie) error {
	if len(cookies) == 0 {
		return nil
	}
	domain := cookies[0].Domain
	return r.saveCookiesToKV(ctx, domain, cookies)
}

// SaveCookies persists cookies to TS-01 KV (configDB).
func (r *browserRuntime) SaveCookies(ctx context.Context, domain string, cookies Cookie) error {
	return r.saveCookiesToKV(ctx, domain, cookies)
}

// LoadCookies retrieves cookies from TS-01 KV (configDB).
func (r *browserRuntime) LoadCookies(ctx context.Context, domain string) (Cookie, error) {
	return r.loadCookiesFromKV(ctx, domain)
}

// ClearCookies removes cookies for a domain from TS-01 KV.
func (r *browserRuntime) ClearCookies(ctx context.Context, domain string) error {
	if r.configDB == nil {
		return fmt.Errorf("credential storage not available")
	}
	key := credentialKey(domain)
	_, err := r.configDB.ExecContext(ctx
		`DELETE FROM kv_store WHERE namespace = ? AND key = ?`
		credentialNamespace, key
	)
	return err
}

// saveCookiesToKV writes cookies as JSON into kv_store.
func (r *browserRuntime) saveCookiesToKV(ctx context.Context, domain string, cookies Cookie) error {
	if r.configDB == nil {
		return fmt.Errorf("credential storage not available")
	}

	data, err := json.Marshal(cookies)
	if err != nil {
		return fmt.Errorf("marshal cookies: %w", err)
	}

	key := credentialKey(domain)
	_, err = r.configDB.ExecContext(ctx, `
		INSERT INTO kv_store (namespace, key, value, updated_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(namespace, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, credentialNamespace, key, string(data))
	return err
}

// loadCookiesFromKV reads cookies JSON from kv_store.
func (r *browserRuntime) loadCookiesFromKV(ctx context.Context, domain string) (Cookie, error) {
	if r.configDB == nil {
		return nil, fmt.Errorf("credential storage not available")
	}

	key := credentialKey(domain)
	var value string
	err := r.configDB.QueryRowContext(ctx
		`SELECT value FROM kv_store WHERE namespace = ? AND key = ?`
		credentialNamespace, key
	).Scan(&value)

	if err == sql.ErrNoRows {
		return Cookie{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load cookies: %w", err)
	}

	var cookies Cookie
	if err := json.Unmarshal(byte(value), &cookies); err != nil {
		return nil, fmt.Errorf("unmarshal cookies: %w", err)
	}
	return cookies, nil
}

// InjectSecureInput directly injects text into a password field via CDP
// bypassing the Agent channel. The value never appears in logs or LLM context.
func (r *browserRuntime) InjectSecureInput(ctx context.Context, taskID int64, selector, value string) error {
	if r.State != StateRunning {
		return ErrBrowserUnavailable
	}

	page, err := r.ensureLiveViewPage(ctx)
	if err != nil {
		return fmt.Errorf("secure input: get page: %w", err)
	}

	el, err := page.Element(selector)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrElementNotFound, selector)
	}

	// Verify it's actually a password field
	inputType, _ := el.Attribute("type")
	if inputType == nil || *inputType != "password" {
		return fmt.Errorf("secure-input: element is not a password field: %s", selector)
	}

	// CDP direct injection — value goes directly to browser, not through JS eval
	if err := el.Focus; err != nil {
		return fmt.Errorf("focus element: %w", err)
	}

	// Use CDP Input.dispatchKeyEvent for each character
	for _, ch := range value {
		evt := proto.InputDispatchKeyEvent{
			Type: proto.InputDispatchKeyEventTypeChar
			Text: string(ch)
		}
		if err := evt.Call(page); err != nil {
			return fmt.Errorf("inject key: %w", err)
		}
	}

	return nil
}
