package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	DefaultTenant  = "consumers"
	DefaultScope   = "https://outlook.office.com/IMAP.AccessAsUser.All https://outlook.office.com/POP.AccessAsUser.All offline_access"
	GoogleScope    = "https://mail.google.com/"
	GoogleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	GoogleTokenURL = "https://oauth2.googleapis.com/token" // #nosec G101 -- public OAuth scope constant; not a credential

	oauthPollFallbackInterval = 5 * time.Second
	oauthReadHeaderTimeout    = 5 * time.Second
	oauthTokenBytes           = 32
	oauthLoopbackTimeout      = 10 * time.Minute
	oauthHTTPTimeout          = 30 * time.Second
	oauthResponseLimit        = 1 << 20
	oauthKeyClientID          = "client_id"
)

// Config contains the public-client settings needed by Microsoft's device flow.
type Config struct {
	Provider         string
	ClientID         string
	Tenant           string
	Scopes           []string
	DeviceCodeURL    string
	AuthorizationURL string
	TokenURL         string
	HTTPClient       *http.Client
	// ValidateAccessToken checks mailbox access before a login replaces saved credentials.
	ValidateAccessToken func(context.Context, string) error
}

// Store keeps OAuth state as one opaque value per account.
type Store interface {
	Get(ctx context.Context, account string) (string, error)
	Set(ctx context.Context, account, value string) error
}

type tokenState struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
}

// DeviceLogin authorizes a Microsoft personal account and stores tokens without
// printing either token. Output contains only the device verification details.
func DeviceLogin(ctx context.Context, cfg *Config, account string, store Store, output io.Writer) error {
	if cfg == nil {
		return errors.New("account, token store, and output are required")
	}
	copied := *cfg
	normalized := copied.normalized()
	cfg = &normalized
	if cfg.Provider != "microsoft" {
		return errors.New("device login is supported only for Microsoft OAuth")
	}
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
	return pollDeviceToken(ctx, cfg, account, store, &device)
}

func pollDeviceToken(ctx context.Context, cfg *Config, account string, store Store, device *deviceCodeResponse) error {
	interval := time.Duration(device.Interval) * time.Second
	if interval <= 0 {
		interval = oauthPollFallbackInterval
	}
	pollCtx, cancel := context.WithTimeout(ctx, time.Duration(device.ExpiresIn)*time.Second)
	defer cancel()
	for {
		if err := waitDeviceInterval(pollCtx, ctx, interval); err != nil {
			return err
		}
		state, err := requestToken(pollCtx, cfg, url.Values{
			"grant_type":     {"urn:ietf:params:oauth:grant-type:device_code"},
			oauthKeyClientID: {cfg.ClientID},
			"device_code":    {device.DeviceCode},
		})
		if err != nil {
			done, next := handleDevicePollError(err, &interval)
			if done {
				return next
			}
			continue
		}
		if state.RefreshToken == "" {
			return errors.New("authorization response did not include a refresh token; ensure offline_access is requested")
		}
		return saveOAuthState(ctx, cfg, store, account, state)
	}
}

func waitDeviceInterval(pollCtx, ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	select {
	case <-pollCtx.Done():
		timer.Stop()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("device authorization expired")
	case <-timer.C:
		return nil
	}
}

func handleDevicePollError(err error, interval *time.Duration) (bool, error) {
	var oauthErr *protocolError
	if errors.As(err, &oauthErr) && oauthErr.Code == "authorization_pending" {
		return false, nil
	}
	if errors.As(err, &oauthErr) && oauthErr.Code == "slow_down" {
		*interval += oauthPollFallbackInterval
		return false, nil
	}
	return true, err
}

func saveOAuthState(ctx context.Context, cfg *Config, store Store, account string, state tokenState) error {
	if cfg.ValidateAccessToken != nil {
		if err := cfg.ValidateAccessToken(ctx, state.AccessToken); err != nil {
			return fmt.Errorf("OAuth authorization completed, but mailbox access could not be verified; new credentials were not saved: %w", err)
		}
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode OAuth state: %w", err)
	}
	if err := store.Set(ctx, account, string(encoded)); err != nil {
		return fmt.Errorf("save OAuth state: %w", err)
	}
	return nil
}

// LoopbackLogin authorizes a Google desktop OAuth client with PKCE and stores
// the resulting token set without printing secrets.
func LoopbackLogin(ctx context.Context, cfg *Config, account string, store Store, output io.Writer) error {
	if cfg == nil {
		return errors.New("account, token store, and output are required")
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, oauthLoopbackTimeout)
		defer cancel()
	}
	copied := *cfg
	normalized := copied.normalized()
	cfg = &normalized
	if cfg.Provider != "google" {
		return errors.New("loopback login is supported only for Google OAuth")
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	if account == "" || store == nil || output == nil {
		return errors.New("account, token store, and output are required")
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("start OAuth loopback listener: %w", err)
	}
	defer func() { _ = listener.Close() }()
	redirectURI := "http://" + listener.Addr().String()
	state, verifier, err := newLoopbackAuth(cfg, redirectURI, output)
	if err != nil {
		return err
	}
	callbackCh := make(chan loopbackCallback, 1)
	server, serveErr := newLoopbackServer(listener, state, callbackCh)
	defer func() { _ = server.Close() }()
	return awaitLoopbackToken(ctx, cfg, account, store, redirectURI, verifier, serveErr, callbackCh)
}

type loopbackCallback struct {
	code string
	err  error
}

func newLoopbackAuth(cfg *Config, redirectURI string, output io.Writer) (string, string, error) {
	state, err := randomURLToken(oauthTokenBytes)
	if err != nil {
		return "", "", fmt.Errorf("create OAuth state: %w", err)
	}
	verifier, err := randomURLToken(oauthTokenBytes)
	if err != nil {
		return "", "", fmt.Errorf("create PKCE verifier: %w", err)
	}
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	authURL, err := buildLoopbackAuthURL(cfg, redirectURI, state, challenge)
	if err != nil {
		return "", "", err
	}
	if _, err := fmt.Fprintf(output, "Open this URL to authorize Mailvault:\n%s\n", authURL.String()); err != nil {
		return "", "", fmt.Errorf("write OAuth instructions: %w", err)
	}
	return state, verifier, nil
}

func buildLoopbackAuthURL(cfg *Config, redirectURI, state, challenge string) (*url.URL, error) {
	authURL, err := url.Parse(cfg.AuthorizationURL)
	if err != nil {
		return nil, fmt.Errorf("parse authorization URL: %w", err)
	}
	query := authURL.Query()
	query.Set(oauthKeyClientID, cfg.ClientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("response_type", "code")
	query.Set("scope", strings.Join(cfg.Scopes, " "))
	query.Set("state", state)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	query.Set("access_type", "offline")
	query.Set("prompt", "consent")
	authURL.RawQuery = query.Encode()
	return authURL, nil
}

func newLoopbackServer(listener net.Listener, state string, callbackCh chan<- loopbackCallback) (*http.Server, <-chan error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", loopbackCallbackHandler(state, callbackCh))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: oauthReadHeaderTimeout}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	return server, serveErr
}

func loopbackCallbackHandler(state string, callbackCh chan<- loopbackCallback) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkLoopbackRequest(w, r, state) {
			return
		}
		if handleLoopbackError(w, r, callbackCh) {
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" || containsControl(code) {
			http.Error(w, "missing authorization code", http.StatusBadRequest)
			sendLoopbackCallback(callbackCh, loopbackCallback{err: errors.New("OAuth callback did not include an authorization code")})
			return
		}
		_, _ = io.WriteString(w, "Authorization received. Return to Mailvault for the mailbox verification result. You may close this window.\n") //nolint:errcheck // best-effort loopback page; failure only means the client disconnected
		sendLoopbackCallback(callbackCh, loopbackCallback{code: code})
	}
}

func checkLoopbackRequest(w http.ResponseWriter, r *http.Request, state string) bool {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return false
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if r.URL.Query().Get("state") != state {
		http.Error(w, "invalid OAuth state", http.StatusBadRequest)
		return false
	}
	return true
}

func handleLoopbackError(w http.ResponseWriter, r *http.Request, callbackCh chan<- loopbackCallback) bool {
	oauthErr := r.URL.Query().Get("error")
	if oauthErr == "" {
		return false
	}
	http.Error(w, "authorization was not completed", http.StatusBadRequest)
	sendLoopbackCallback(callbackCh, loopbackCallback{err: &protocolError{Code: safeCode(oauthErr, http.StatusBadRequest)}})
	return true
}

func sendLoopbackCallback(ch chan<- loopbackCallback, result loopbackCallback) {
	select {
	case ch <- result:
	default:
	}
}

func awaitLoopbackToken(ctx context.Context, cfg *Config, account string, store Store, redirectURI, verifier string, serveErr <-chan error, callbackCh <-chan loopbackCallback) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-serveErr:
		return fmt.Errorf("serve OAuth callback: %w", err)
	case result := <-callbackCh:
		return exchangeLoopbackToken(ctx, cfg, account, store, redirectURI, verifier, result)
	}
}

func exchangeLoopbackToken(ctx context.Context, cfg *Config, account string, store Store, redirectURI, verifier string, result loopbackCallback) error {
	if result.err != nil {
		return result.err
	}
	state, err := requestToken(ctx, cfg, url.Values{"grant_type": {"authorization_code"}, oauthKeyClientID: {cfg.ClientID}, "code": {result.code}, "redirect_uri": {redirectURI}, "code_verifier": {verifier}})
	if err != nil {
		return err
	}
	if state.RefreshToken == "" {
		return errors.New("authorization response did not include a refresh token; retry consent with offline access")
	}
	return saveOAuthState(ctx, cfg, store, account, state)
}

func randomURLToken(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

type Provider struct {
	cfg     Config
	account string
	store   Store
	gate    chan struct{}
}

// NewTokenProvider returns a concurrency-safe provider that refreshes tokens
// before expiry and atomically saves rotated refresh tokens through Store.Set.
func NewTokenProvider(cfg *Config, account string, store Store) (*Provider, error) {
	normalized := cfg.normalized()
	cfg = &normalized
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if account == "" || store == nil {
		return nil, errors.New("account and token store are required")
	}
	return &Provider{cfg: *cfg, account: account, store: store, gate: make(chan struct{}, 1)}, nil
}

func (provider *Provider) AccessToken(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	select {
	case provider.gate <- struct{}{}:
		defer func() { <-provider.gate }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	encoded, err := provider.store.Get(ctx, provider.account)
	if err != nil {
		return "", fmt.Errorf("read OAuth state: %w", err)
	}
	var state tokenState
	if err := json.Unmarshal([]byte(encoded), &state); err != nil || state.RefreshToken == "" {
		return "", errors.New("OAuth state missing or invalid; run mailvault auth login")
	}
	if state.AccessToken != "" && time.Until(state.Expiry) > time.Minute {
		return state.AccessToken, nil
	}
	refreshed, err := requestToken(ctx, &provider.cfg, url.Values{
		"grant_type":     {"refresh_token"},
		oauthKeyClientID: {provider.cfg.ClientID},
		"refresh_token":  {state.RefreshToken},
		"scope":          {strings.Join(provider.cfg.Scopes, " ")},
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
	if err := provider.store.Set(ctx, provider.account, string(encodedState)); err != nil {
		return "", fmt.Errorf("save refreshed OAuth state: %w", err)
	}
	return refreshed.AccessToken, nil
}

func (cfg *Config) normalized() Config {
	if cfg.Provider == "" {
		cfg.Provider = "microsoft"
	}
	switch cfg.Provider {
	case "google":
		if len(cfg.Scopes) == 0 {
			cfg.Scopes = []string{GoogleScope}
		}
		if cfg.AuthorizationURL == "" {
			cfg.AuthorizationURL = GoogleAuthURL
		}
		if cfg.TokenURL == "" {
			cfg.TokenURL = GoogleTokenURL
		}
	default:
		if cfg.Tenant == "" {
			cfg.Tenant = DefaultTenant
		}
		if len(cfg.Scopes) == 0 {
			cfg.Scopes = strings.Fields(DefaultScope)
		}
		base := "https://login.microsoftonline.com/" + url.PathEscape(cfg.Tenant) + "/oauth2/v2.0/"
		if cfg.DeviceCodeURL == "" {
			cfg.DeviceCodeURL = base + "devicecode"
		}
		if cfg.TokenURL == "" {
			cfg.TokenURL = base + "token"
		}
	}
	client := &http.Client{Timeout: oauthHTTPTimeout}
	if cfg.HTTPClient != nil {
		*client = *cfg.HTTPClient
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	cfg.HTTPClient = client
	return *cfg
}

func (cfg *Config) validate() error {
	if strings.TrimSpace(cfg.ClientID) == "" {
		return errors.New("OAuth2 client_id is required")
	}
	endpoints := []string{cfg.TokenURL}
	switch cfg.Provider {
	case "google":
		endpoints = append(endpoints, cfg.AuthorizationURL)
	case "microsoft":
		endpoints = append(endpoints, cfg.DeviceCodeURL)
	default:
		return errors.New("OAuth2 provider must be microsoft or google")
	}
	for _, endpoint := range endpoints {
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

func requestDeviceCode(ctx context.Context, cfg *Config) (deviceCodeResponse, error) {
	form := url.Values{oauthKeyClientID: {cfg.ClientID}, "scope": {strings.Join(cfg.Scopes, " ")}}
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
	if err := decodeOAuthResponse(ctx, resp.Body, &result, "device authorization"); err != nil {
		return deviceCodeResponse{}, err
	}
	if resp.StatusCode/100 != 2 || result.Error != "" {
		return deviceCodeResponse{}, &protocolError{Code: safeCode(result.Error, resp.StatusCode)}
	}
	if err := validateDeviceCodeResponse(&result); err != nil {
		return deviceCodeResponse{}, err
	}
	return result, nil
}

func validateDeviceCodeResponse(result *deviceCodeResponse) error {
	if result.DeviceCode == "" || result.UserCode == "" || result.VerificationURI == "" || result.ExpiresIn <= 0 {
		return errors.New("incomplete device authorization response")
	}
	verificationURI, err := url.Parse(result.VerificationURI)
	if err != nil || verificationURI.Scheme != "https" || verificationURI.Host == "" || verificationURI.User != nil || containsControl(result.UserCode+result.VerificationURI) || !deviceCodePattern.MatchString(result.UserCode) {
		return errors.New("invalid device authorization instructions")
	}
	return nil
}

func requestToken(ctx context.Context, cfg *Config, form url.Values) (tokenState, error) {
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
	if err := decodeOAuthResponse(ctx, resp.Body, &result, "OAuth token"); err != nil {
		return tokenState{}, err
	}
	if resp.StatusCode/100 != 2 || result.Error != "" {
		return tokenState{}, &protocolError{Code: safeCode(result.Error, resp.StatusCode)}
	}
	if result.AccessToken == "" || result.ExpiresIn <= 0 {
		return tokenState{}, errors.New("incomplete OAuth token response")
	}
	return tokenState{AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, Expiry: time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)}, nil
}

func decodeOAuthResponse(ctx context.Context, body io.Reader, result any, kind string) error {
	err := json.NewDecoder(io.LimitReader(body, oauthResponseLimit)).Decode(result)
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("read %s response: %w", kind, err)
	}
	return fmt.Errorf("invalid %s response", kind)
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
