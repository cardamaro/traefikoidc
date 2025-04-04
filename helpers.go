package traefikoidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

// generateNonce creates a cryptographically secure random nonce
// for use in the OIDC authentication flow. The nonce is used to
// prevent replay attacks by ensuring the token received matches
// the authentication request.
func generateNonce() (string, error) {
	nonceBytes := make([]byte, 32)
	_, err := rand.Read(nonceBytes)
	if err != nil {
		return "", fmt.Errorf("could not generate nonce: %w", err)
	}
	return base64.URLEncoding.EncodeToString(nonceBytes), nil
}

// generateCodeVerifier creates a cryptographically secure random string
// for use as a PKCE code verifier. The code verifier must be between 43 and 128
// characters long, per the PKCE spec (RFC 7636).
func generateCodeVerifier() (string, error) {
	// Using 32 bytes (256 bits) will produce a 43 character base64url string
	verifierBytes := make([]byte, 32)
	_, err := rand.Read(verifierBytes)
	if err != nil {
		return "", fmt.Errorf("could not generate code verifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(verifierBytes), nil
}

// deriveCodeChallenge creates a code challenge from a code verifier
// using the SHA-256 method as specified in the PKCE standard (RFC 7636).
func deriveCodeChallenge(codeVerifier string) string {
	// Calculate SHA-256 hash of the code verifier
	hasher := sha256.New()
	hasher.Write([]byte(codeVerifier))
	hash := hasher.Sum(nil)

	// Base64url encode the hash to get the code challenge
	return base64.RawURLEncoding.EncodeToString(hash)
}

// TokenResponse represents the response from the OIDC token endpoint.
// It contains the various tokens and metadata returned after successful
// code exchange or token refresh operations.
type TokenResponse struct {
	// IDToken is the OIDC ID token containing user claims
	IDToken string `json:"id_token"`

	// AccessToken is the OAuth 2.0 access token for API access
	AccessToken string `json:"access_token"`

	// RefreshToken is the OAuth 2.0 refresh token for obtaining new tokens
	RefreshToken string `json:"refresh_token"`

	// ExpiresIn is the lifetime in seconds of the access token
	ExpiresIn int `json:"expires_in"`

	// TokenType is the type of token, typically "Bearer"
	TokenType string `json:"token_type"`
}

// exchangeTokens performs the OAuth 2.0 token exchange with the OIDC provider.
// It supports both authorization code and refresh token grant types.
// Parameters:
//   - ctx: Context for the HTTP request
//   - grantType: The OAuth 2.0 grant type ("authorization_code" or "refresh_token")
//   - codeOrToken: Either the authorization code or refresh token
//   - redirectURL: The callback URL for authorization code grant
//   - codeVerifier: Optional PKCE code verifier for authorization code grant
func (t *TraefikOidc) exchangeTokens(ctx context.Context, grantType string, codeOrToken string, redirectURL string, codeVerifier string) (*TokenResponse, error) {
	data := url.Values{
		"grant_type":    {grantType},
		"client_id":     {t.clientID},
		"client_secret": {t.clientSecret},
	}

	if grantType == "authorization_code" {
		data.Set("code", codeOrToken)
		data.Set("redirect_uri", redirectURL)

		// Add code_verifier if PKCE is being used
		if codeVerifier != "" {
			data.Set("code_verifier", codeVerifier)
		}
	} else if grantType == "refresh_token" {
		data.Set("refresh_token", codeOrToken)
	}

	// Create a cookie jar for this request to handle redirects with cookies
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Transport: t.httpClient.Transport,
		Timeout:   t.httpClient.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Always follow redirects for OIDC endpoints
			if len(via) >= 50 {
				return fmt.Errorf("stopped after 50 redirects")
			}
			return nil
		},
		Jar: jar,
	}

	req, err := http.NewRequestWithContext(ctx, "POST", t.tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to exchange tokens: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token endpoint returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var tokenResponse TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResponse); err != nil {
		return nil, fmt.Errorf("failed to decode token response: %w", err)
	}

	return &tokenResponse, nil
}

// getNewTokenWithRefreshToken obtains new tokens using a refresh token.
// This is used to refresh access tokens before they expire.
func (t *TraefikOidc) getNewTokenWithRefreshToken(refreshToken string) (*TokenResponse, error) {
	ctx := context.Background()
	tokenResponse, err := t.exchangeTokens(ctx, "refresh_token", refreshToken, "", "")
	if err != nil {
		return nil, fmt.Errorf("failed to refresh token: %w", err)
	}

	t.logger.Debugf("Token response: %+v", tokenResponse)
	return tokenResponse, nil
}

// extractClaims parses a JWT token and extracts its claims.
// It handles base64url decoding and JSON parsing of the token payload.
func extractClaims(tokenString string) (map[string]interface{}, error) {
	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid token format")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("failed to decode token payload: %w", err)
	}

	var claims map[string]interface{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("failed to unmarshal claims: %w", err)
	}

	return claims, nil
}

// TokenCache provides a caching mechanism for validated tokens.
// It stores token claims to avoid repeated validation of the
// same token, improving performance for frequently used tokens.
type TokenCache struct {
	// cache is the underlying cache implementation
	cache *Cache
}

// NewTokenCache creates a new TokenCache instance.
func NewTokenCache() *TokenCache {
	return &TokenCache{
		cache: NewCache(),
	}
}

// Set stores a token's claims in the cache with an expiration time.
func (tc *TokenCache) Set(token string, claims map[string]interface{}, expiration time.Duration) {
	token = "t-" + token
	tc.cache.Set(token, claims, expiration)
}

// Get retrieves a token's claims from the cache.
// Returns the claims and a boolean indicating if the token was found.
func (tc *TokenCache) Get(token string) (map[string]interface{}, bool) {
	token = "t-" + token
	value, found := tc.cache.Get(token)
	if !found {
		return nil, false
	}
	claims, ok := value.(map[string]interface{})
	return claims, ok
}

// Delete removes a token from the cache.
func (tc *TokenCache) Delete(token string) {
	token = "t-" + token
	tc.cache.Delete(token)
}

// Cleanup removes expired tokens from the cache.
func (tc *TokenCache) Cleanup() {
	tc.cache.Cleanup()
}

// exchangeCodeForToken exchanges an authorization code for tokens.
// It handles PKCE (Proof Key for Code Exchange) based on middleware configuration.
// The code verifier is only included in the token request if PKCE is enabled.
func (t *TraefikOidc) exchangeCodeForToken(code string, redirectURL string, codeVerifier string) (*TokenResponse, error) {
	ctx := context.Background()

	// Only include code verifier if PKCE is enabled
	effectiveCodeVerifier := ""
	if t.enablePKCE && codeVerifier != "" {
		effectiveCodeVerifier = codeVerifier
	}

	tokenResponse, err := t.exchangeTokens(ctx, "authorization_code", code, redirectURL, effectiveCodeVerifier)
	if err != nil {
		return nil, fmt.Errorf("failed to exchange code for token: %w", err)
	}
	return tokenResponse, nil
}

// createStringMap creates a map from a slice of strings.
// Used for efficient lookups in allowed domains and roles.
func createStringMap(keys []string) map[string]struct{} {
	result := make(map[string]struct{})
	for _, key := range keys {
		result[key] = struct{}{}
	}
	return result
}

// handleLogout manages the OIDC logout process.
// It clears the session and redirects either to the OIDC provider's
// end session endpoint (if available) or to the configured post-logout URL.
func (t *TraefikOidc) handleLogout(rw http.ResponseWriter, req *http.Request) {
	session, err := t.sessionManager.GetSession(req)
	if err != nil {
		t.logger.Errorf("Error getting session: %v", err)
		http.Error(rw, "Session error", http.StatusInternalServerError)
		return
	}

	accessToken := session.GetAccessToken()

	if err := session.Clear(req, rw); err != nil {
		t.logger.Errorf("Error clearing session: %v", err)
		http.Error(rw, "Session error", http.StatusInternalServerError)
		return
	}

	host := t.determineHost(req)
	scheme := t.determineScheme(req)
	baseURL := fmt.Sprintf("%s://%s", scheme, host)

	postLogoutRedirectURI := t.postLogoutRedirectURI
	if postLogoutRedirectURI == "" {
		postLogoutRedirectURI = fmt.Sprintf("%s/", baseURL)
	} else if !strings.HasPrefix(postLogoutRedirectURI, "http") {
		postLogoutRedirectURI = fmt.Sprintf("%s%s", baseURL, postLogoutRedirectURI)
	}

	if t.endSessionURL != "" && accessToken != "" {
		logoutURL, err := BuildLogoutURL(t.endSessionURL, accessToken, postLogoutRedirectURI)
		if err != nil {
			t.logger.Errorf("Failed to build logout URL: %v", err)
			http.Error(rw, "Logout error", http.StatusInternalServerError)
			return
		}
		http.Redirect(rw, req, logoutURL, http.StatusFound)
		return
	}

	http.Redirect(rw, req, postLogoutRedirectURI, http.StatusFound)
}

// BuildLogoutURL constructs the OIDC end session URL with appropriate parameters.
// Parameters:
//   - endSessionURL: The OIDC provider's end session endpoint
//   - idToken: The ID token to be invalidated
//   - postLogoutRedirectURI: Where to redirect after logout completes
func BuildLogoutURL(endSessionURL, idToken, postLogoutRedirectURI string) (string, error) {
	u, err := url.Parse(endSessionURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse end session URL: %w", err)
	}

	q := u.Query()
	q.Set("id_token_hint", idToken)
	if postLogoutRedirectURI != "" {
		q.Set("post_logout_redirect_uri", postLogoutRedirectURI)
	}
	u.RawQuery = q.Encode()

	return u.String(), nil
}
