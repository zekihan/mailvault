package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	DefaultTenant = "consumers"
	DefaultScope  = "https://outlook.office.com/IMAP.AccessAsUser.All https://outlook.office.com/POP.AccessAsUser.All offline_access"
)

// Config contains the public-client settings needed by Microsoft's device flow.
type Config struct {
	ClientID      string
	Tenant        string
	Scopes        []string
	DeviceCodeURL string
	TokenURL      string
	HTTPClient    *http.Client
}

// Store keeps OAuth state as one opaque value per account.
type Store interface {
	Get(context.Context, string) (string, error)
	Set(context.Context, string, string) error
}

type tokenState struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
}

// DeviceLogin authorizes a Microsoft personal account and stores tokens without
// printing either token. Output contains only the device verification details.
func DeviceLogin(ctx context.Context, cfg Config, account string, store Store, output io.Writer) error {
	cfg = cfg.normalized()
	if err := cfg.validate(); err != nil {
		return err
	}
	if account == "" || store == nil || output == nil {
		return errors.New("account, token store, and output are required")
	}
	device, err := requestDeviceCode(ctx, cfg)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "Open %s and enter code %s\n", device.VerificationURI, device.UserCode); err != nil {
		return fmt.Errorf("write device instructions: %w", err)
	}
	interval := time.Duration(device.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	pollCtx, cancel := context.WithTimeout(ctx, time.Duration(device.ExpiresIn)*time.Second)
	defer cancel()
	for {
		timer := time.NewTimer(interval)
		select {
		case <-pollCtx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("device authorization expired")
		case <-timer.C:
		}
		state, err := requestToken(pollCtx, cfg, url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"client_id":   {cfg.ClientID},
			"device_code": {device.DeviceCode},
		})
		if err != nil {
			var oauthErr *protocolError
			if errors.As(err, &oauthErr) && oauthErr.Code == "authorization_pending" {
				continue
			}
			if errors.As(err, &oauthErr) && oauthErr.Code == "slow_down" {
				interval += 5 * time.Second
				continue
			}
			return err
		}
		if state.RefreshToken == "" {
			return errors.New("authorization response did not include a refresh token; ensure offline_access is requested")
		}
		encoded, err := json.Marshal(state)
		if err != nil {
			return fmt.Errorf("encode OAuth state: %w", err)
		}
		if err := store.Set(ctx, account, string(encoded)); err != nil {
			return fmt.Errorf("save OAuth state to keyring: %w", err)
		}
		return nil
	}
}

// NewTokenProvider returns a concurrency-safe provider that refreshes tokens
// before expiry and atomically saves rotated refresh tokens through Store.Set.
func NewTokenProvider(cfg Config, account string, store Store) (*Provider, error) {
	cfg = cfg.normalized()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if account == "" || store == nil {
		return nil, errors.New("account and token store are required")
	}
	return &Provider{cfg: cfg, account: account, store: store, gate: make(chan struct{}, 1)}, nil
}

type Provider struct {
	cfg     Config
	account string
	store   Store
	gate    chan struct{}
}

func (p *Provider) AccessToken(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	select {
	case p.gate <- struct{}{}:
		defer func() { <-p.gate }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	encoded, err := p.store.Get(ctx, p.account)
	if err != nil {
		return "", fmt.Errorf("read OAuth state from keyring: %w", err)
	}
	var state tokenState
	if err := json.Unmarshal([]byte(encoded), &state); err != nil || state.RefreshToken == "" {
		return "", errors.New("OAuth state missing or invalid; run mailvault auth login")
	}
	if state.AccessToken != "" && time.Until(state.Expiry) > time.Minute {
		return state.AccessToken, nil
	}
	refreshed, err := requestToken(ctx, p.cfg, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {p.cfg.ClientID},
		"refresh_token": {state.RefreshToken},
		"scope":         {strings.Join(p.cfg.Scopes, " ")},
	})
	if err != nil {
		return "", err
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = state.RefreshToken
	}
	encodedState, err := json.Marshal(refreshed)
	if err != nil {
		return "", fmt.Errorf("encode OAuth state: %w", err)
	}
	if err := p.store.Set(ctx, p.account, string(encodedState)); err != nil {
		return "", fmt.Errorf("save refreshed OAuth state to keyring: %w", err)
	}
	return refreshed.AccessToken, nil
}

func (c Config) normalized() Config {
	if c.Tenant == "" {
		c.Tenant = DefaultTenant
	}
	if len(c.Scopes) == 0 {
		c.Scopes = strings.Fields(DefaultScope)
	}
	base := "https://login.microsoftonline.com/" + url.PathEscape(c.Tenant) + "/oauth2/v2.0/"
	if c.DeviceCodeURL == "" {
		c.DeviceCodeURL = base + "devicecode"
	}
	if c.TokenURL == "" {
		c.TokenURL = base + "token"
	}
	client := &http.Client{Timeout: 30 * time.Second}
	if c.HTTPClient != nil {
		*client = *c.HTTPClient
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	c.HTTPClient = client
	return c
}

func (c Config) validate() error {
	if strings.TrimSpace(c.ClientID) == "" {
		return errors.New("OAuth2 client_id is required")
	}
	for _, endpoint := range []string{c.DeviceCodeURL, c.TokenURL} {
		u, err := url.Parse(endpoint)
		if err != nil || (!httpsURL(u.Scheme) && !httpTestURL(u.Scheme, u.Hostname())) || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("OAuth2 endpoints must use HTTPS (HTTP is allowed only for localhost tests)")
		}
	}
	return nil
}

type deviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
	Error           string `json:"error"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
}

type protocolError struct{ Code string }

func (e *protocolError) Error() string { return "OAuth authorization failed: " + e.Code }

func requestDeviceCode(ctx context.Context, cfg Config) (deviceCodeResponse, error) {
	form := url.Values{"client_id": {cfg.ClientID}, "scope": {strings.Join(cfg.Scopes, " ")}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.DeviceCodeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return deviceCodeResponse{}, fmt.Errorf("create device authorization request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return deviceCodeResponse{}, fmt.Errorf("request device authorization: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var result deviceCodeResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return deviceCodeResponse{}, errors.New("invalid device authorization response")
	}
	if resp.StatusCode/100 != 2 || result.Error != "" {
		return deviceCodeResponse{}, &protocolError{Code: safeCode(result.Error, resp.StatusCode)}
	}
	if result.DeviceCode == "" || result.UserCode == "" || result.VerificationURI == "" || result.ExpiresIn <= 0 {
		return deviceCodeResponse{}, errors.New("incomplete device authorization response")
	}
	verificationURI, err := url.Parse(result.VerificationURI)
	if err != nil || verificationURI.Scheme != "https" || verificationURI.Host == "" || verificationURI.User != nil || containsControl(result.UserCode+result.VerificationURI) || !deviceCodePattern.MatchString(result.UserCode) {
		return deviceCodeResponse{}, errors.New("invalid device authorization instructions")
	}
	return result, nil
}

func requestToken(ctx context.Context, cfg Config, form url.Values) (tokenState, error) {
	if form.Get("scope") == "" {
		form.Set("scope", strings.Join(cfg.Scopes, " "))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenState{}, fmt.Errorf("create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return tokenState{}, fmt.Errorf("request OAuth token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var result tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return tokenState{}, errors.New("invalid OAuth token response")
	}
	if resp.StatusCode/100 != 2 || result.Error != "" {
		return tokenState{}, &protocolError{Code: safeCode(result.Error, resp.StatusCode)}
	}
	if result.AccessToken == "" || result.ExpiresIn <= 0 {
		return tokenState{}, errors.New("incomplete OAuth token response")
	}
	return tokenState{AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, Expiry: time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)}, nil
}

func safeCode(code string, status int) string {
	switch code {
	case "authorization_pending", "slow_down", "authorization_declined", "bad_verification_code", "expired_token", "invalid_grant", "invalid_client", "invalid_request", "unauthorized_client", "invalid_scope", "interaction_required":
		return code
	default:
		return fmt.Sprintf("HTTP %d", status)
	}
}

var deviceCodePattern = regexp.MustCompile(`^[A-Za-z0-9-]{4,32}$`)

func httpsURL(scheme string) bool { return scheme == "https" }

func httpTestURL(scheme, hostname string) bool {
	return scheme == "http" && (hostname == "localhost" || hostname == "127.0.0.1" || hostname == "::1")
}

func containsControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
