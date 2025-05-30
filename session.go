package traefikoidc

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/sessions"
)

// generateSecureRandomString creates a cryptographically secure, hex-encoded random string.
// It reads the specified number of bytes from crypto/rand and encodes them as a hexadecimal string.
//
// Parameters:
//   - length: The number of random bytes to generate (the resulting hex string will be twice this length).
//
// Returns:
//   - A hex-encoded random string.
//   - An error if reading random bytes fails.
func generateSecureRandomString(length int) (string, error) {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}

// Cookie names and configuration constants used for session management
const (
	// Using fixed prefixes for consistent cookie naming across restarts
	mainCookieName     = "_oidc_raczylo_m"
	accessTokenCookie  = "_oidc_raczylo_a"
	refreshTokenCookie = "_oidc_raczylo_r"
)

const (
	// maxCookieSize is the maximum size for each cookie chunk.
	// This value is calculated to ensure the final cookie size stays within browser limits:
	// 1. Browser cookie size limit is typically 4096 bytes
	// 2. Cookie content undergoes encryption (adds 28 bytes) and base64 encoding (4/3 ratio)
	// 3. Calculation:
	//    - Let x be the chunk size
	//    - After encryption: x + 28 bytes
	//    - After base64: ((x + 28) * 4/3) bytes
	//    - Must satisfy: ((x + 28) * 4/3) ≤ 4096
	//    - Solving for x: x ≤ 3044
	// 4. We use 2000 as a conservative limit to account for cookie metadata
	maxCookieSize = 2000

	// absoluteSessionTimeout defines the maximum lifetime of a session
	// regardless of activity (24 hours)
	absoluteSessionTimeout = 24 * time.Hour

	// minEncryptionKeyLength defines the minimum length for the encryption key
	minEncryptionKeyLength = 32
)

// compressToken compresses the input string using gzip and then encodes the result using standard base64 encoding.
// If any error occurs during compression, it returns the original uncompressed token as a fallback.
//
// Parameters:
//   - token: The string to compress.
//
// Returns:
//   - The base64 encoded, gzipped string, or the original string if compression fails.
func compressToken(token string) string {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	if _, err := gz.Write([]byte(token)); err != nil {
		return token // fallback to uncompressed on error
	}
	if err := gz.Close(); err != nil {
		return token
	}
	return base64.StdEncoding.EncodeToString(b.Bytes())
}

// decompressToken decodes a standard base64 encoded string and then decompresses the result using gzip.
// If base64 decoding or gzip decompression fails, it returns the original input string as a fallback,
// assuming it might not have been compressed.
//
// Parameters:
//   - compressed: The base64 encoded, gzipped string.
//
// Returns:
//   - The decompressed original string, or the input string if decompression fails.
func decompressToken(compressed string) string {
	data, err := base64.StdEncoding.DecodeString(compressed)
	if err != nil {
		return compressed // return as-is if not base64
	}

	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return compressed
	}
	defer gz.Close()

	decompressed, err := io.ReadAll(gz)
	if err != nil {
		return compressed
	}

	return string(decompressed)
}

// SessionManager handles the management of multiple session cookies for OIDC authentication.
// It provides functionality for storing and retrieving authentication state, tokens,
// and other session-related data across multiple cookies.
type SessionManager struct {
	// store is the underlying session store for cookie management.
	store sessions.Store

	// forceHTTPS enforces secure cookie attributes regardless of request scheme.
	forceHTTPS bool

	// logger provides structured logging capabilities.
	logger *Logger

	// sessionPool is a sync.Pool for reusing SessionData objects.
	sessionPool sync.Pool
}

// NewSessionManager creates a new session manager with the specified configuration.
// Parameters:
//   - encryptionKey: Key used to encrypt session data (must be at least 32 bytes)
//   - forceHTTPS: When true, forces secure cookie attributes regardless of request scheme
//   - logger: Logger instance for recording session-related events
//
// Returns an error if the encryption key does not meet minimum length requirements.
func NewSessionManager(encryptionKey string, forceHTTPS bool, logger *Logger) (*SessionManager, error) {
	// Validate encryption key length.
	if len(encryptionKey) < minEncryptionKeyLength {
		return nil, fmt.Errorf("encryption key must be at least %d bytes long", minEncryptionKeyLength)
	}

	sm := &SessionManager{
		store:      sessions.NewCookieStore([]byte(encryptionKey)),
		forceHTTPS: forceHTTPS,
		logger:     logger,
	}

	// Initialize session pool.
	sm.sessionPool.New = func() interface{} {
		// Initialize SessionData with necessary fields and the mutex.
		return &SessionData{
			manager:            sm,
			accessTokenChunks:  make(map[int]*sessions.Session),
			refreshTokenChunks: make(map[int]*sessions.Session),
			refreshMutex:       sync.Mutex{}, // Initialize the mutex
		}
	}

	return sm, nil
}

// getSessionOptions returns a sessions.Options struct configured with security best practices.
// It sets HttpOnly to true, Secure based on the request scheme or forceHTTPS setting,
// SameSite to LaxMode, MaxAge to the absoluteSessionTimeout, and Path to "/".
//
// Parameters:
//   - isSecure: A boolean indicating if the current request context is secure (HTTPS).
//
// Returns:
//   - A pointer to a configured sessions.Options struct.
func (sm *SessionManager) getSessionOptions(isSecure bool) *sessions.Options {
	return &sessions.Options{
		HttpOnly: true,
		Secure:   isSecure || sm.forceHTTPS,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(absoluteSessionTimeout.Seconds()),
		Path:     "/",
	}
}

// GetSession retrieves all session data for the current request.
// It loads the main session and token sessions, including any chunked token data,
// and combines them into a single SessionData structure for easy access.
// Returns an error if any session component cannot be loaded.
func (sm *SessionManager) GetSession(r *http.Request) (*SessionData, error) {
	// Get session from pool.
	sessionData := sm.sessionPool.Get().(*SessionData)
	sessionData.request = r

	var err error
	sessionData.mainSession, err = sm.store.Get(r, mainCookieName)
	if err != nil {
		sm.sessionPool.Put(sessionData)
		return nil, fmt.Errorf("failed to get main session: %w", err)
	}

	// Check for absolute session timeout.
	if createdAt, ok := sessionData.mainSession.Values["created_at"].(int64); ok {
		if time.Since(time.Unix(createdAt, 0)) > absoluteSessionTimeout {
			sessionData.Clear(r, nil)
			return nil, fmt.Errorf("session expired")
		}
	}

	sessionData.accessSession, err = sm.store.Get(r, accessTokenCookie)
	if err != nil {
		sm.sessionPool.Put(sessionData)
		return nil, fmt.Errorf("failed to get access token session: %w", err)
	}

	sessionData.refreshSession, err = sm.store.Get(r, refreshTokenCookie)
	if err != nil {
		sm.sessionPool.Put(sessionData)
		return nil, fmt.Errorf("failed to get refresh token session: %w", err)
	}

	// Clear and reuse chunk maps.
	for k := range sessionData.accessTokenChunks {
		delete(sessionData.accessTokenChunks, k)
	}
	for k := range sessionData.refreshTokenChunks {
		delete(sessionData.refreshTokenChunks, k)
	}

	// Retrieve chunked token sessions.
	sm.getTokenChunkSessions(r, accessTokenCookie, sessionData.accessTokenChunks)
	sm.getTokenChunkSessions(r, refreshTokenCookie, sessionData.refreshTokenChunks)

	return sessionData, nil
}

// getTokenChunkSessions retrieves all cookie chunks associated with a large token (access or refresh).
// It iteratively attempts to load cookies named "{baseName}_0", "{baseName}_1", etc., until
// a cookie is not found or returns an error. The loaded sessions are stored in the provided chunks map.
//
// Parameters:
//   - r: The incoming HTTP request containing the cookies.
//   - baseName: The base name of the cookie (e.g., accessTokenCookie).
//   - chunks: The map (typically SessionData.accessTokenChunks or SessionData.refreshTokenChunks) to populate with the found session chunks.
func (sm *SessionManager) getTokenChunkSessions(r *http.Request, baseName string, chunks map[int]*sessions.Session) {
	for i := 0; ; i++ {
		sessionName := fmt.Sprintf("%s_%d", baseName, i)
		session, err := sm.store.Get(r, sessionName)
		if err != nil || session.IsNew {
			break
		}
		chunks[i] = session
	}
}

// SessionData holds all session information for an authenticated user.
// It manages multiple session cookies to handle the main session state
// and potentially large access and refresh tokens that may need to be
// split across multiple cookies due to browser size limitations.
type SessionData struct {
	// manager is the SessionManager that created this SessionData.
	manager *SessionManager

	// request is the current HTTP request associated with this session.
	request *http.Request

	// mainSession stores authentication state and basic user info.
	mainSession *sessions.Session

	// accessSession stores the primary access token cookie.
	accessSession *sessions.Session

	// refreshSession stores the primary refresh token cookie.
	refreshSession *sessions.Session

	// accessTokenChunks stores additional chunks of the access token
	// when it exceeds the maximum cookie size.
	accessTokenChunks map[int]*sessions.Session

	// refreshTokenChunks stores additional chunks of the refresh token
	// when it exceeds the maximum cookie size.
	refreshTokenChunks map[int]*sessions.Session

	// refreshMutex protects refresh token operations within this session instance.
	refreshMutex sync.Mutex
}

// Save persists all parts of the session (main, access token, refresh token, and any chunks)
// back to the client as cookies in the HTTP response. It applies secure cookie options
// obtained via getSessionOptions based on the request's security context.
//
// Parameters:
//   - r: The original HTTP request (used to determine security context for cookie options).
//   - w: The HTTP response writer to which the Set-Cookie headers will be added.
//
// Returns:
//   - An error if saving any of the session components fails.
func (sd *SessionData) Save(r *http.Request, w http.ResponseWriter) error {
	isSecure := strings.HasPrefix(r.URL.Scheme, "https") || sd.manager.forceHTTPS

	// Set options for all sessions.
	options := sd.manager.getSessionOptions(isSecure)
	sd.mainSession.Options = options
	sd.accessSession.Options = options
	sd.refreshSession.Options = options

	// Save main session.
	if err := sd.mainSession.Save(r, w); err != nil {
		return fmt.Errorf("failed to save main session: %w", err)
	}

	// Save access token session.
	if err := sd.accessSession.Save(r, w); err != nil {
		return fmt.Errorf("failed to save access token session: %w", err)
	}

	// Save refresh token session.
	if err := sd.refreshSession.Save(r, w); err != nil {
		return fmt.Errorf("failed to save refresh token session: %w", err)
	}

	// Save access token chunks.
	for _, session := range sd.accessTokenChunks {
		session.Options = options
		if err := session.Save(r, w); err != nil {
			return fmt.Errorf("failed to save access token chunk session: %w", err)
		}
	}

	// Save refresh token chunks.
	for _, session := range sd.refreshTokenChunks {
		session.Options = options
		if err := session.Save(r, w); err != nil {
			return fmt.Errorf("failed to save refresh token chunk session: %w", err)
		}
	}

	return nil
}

// Clear removes all session data associated with this SessionData instance.
// It clears the values map of the main, access, and refresh sessions, sets their MaxAge to -1
// to expire the cookies immediately, and clears any associated token chunk cookies.
// If a ResponseWriter is provided, it attempts to save the expired sessions to send the
// expiring Set-Cookie headers. Finally, it clears internal fields and returns the SessionData
// object to the pool.
//
// Parameters:
//   - r: The HTTP request (required by the underlying session store).
//   - w: The HTTP response writer (optional). If provided, expiring Set-Cookie headers will be sent.
//
// Returns:
//   - An error if saving the expired sessions fails (only if w is not nil).
func (sd *SessionData) Clear(r *http.Request, w http.ResponseWriter) error {
	// Clear and expire all sessions.
	sd.mainSession.Options.MaxAge = -1
	sd.accessSession.Options.MaxAge = -1
	sd.refreshSession.Options.MaxAge = -1

	for k := range sd.mainSession.Values {
		delete(sd.mainSession.Values, k)
	}
	for k := range sd.accessSession.Values {
		delete(sd.accessSession.Values, k)
	}
	for k := range sd.refreshSession.Values {
		delete(sd.refreshSession.Values, k)
	}

	// Clear chunk sessions.
	sd.clearTokenChunks(r, sd.accessTokenChunks)
	sd.clearTokenChunks(r, sd.refreshTokenChunks)

	var err error
	if w != nil {
		err = sd.Save(r, w)
	}

	// Clear transient per-request fields.
	sd.request = nil

	// Return session to pool.
	sd.manager.sessionPool.Put(sd)

	return err
}

// clearTokenChunks iterates through a map of session chunks, clears their values,
// and sets their MaxAge to -1 to expire them. This is used internally by Clear.
//
// Parameters:
//   - r: The HTTP request (required by the underlying session store, though not directly used here).
//   - chunks: The map of session chunks (e.g., sd.accessTokenChunks) to clear and expire.
func (sd *SessionData) clearTokenChunks(r *http.Request, chunks map[int]*sessions.Session) {
	for _, session := range chunks {
		session.Options.MaxAge = -1
		for k := range session.Values {
			delete(session.Values, k)
		}
	}
}

// GetAuthenticated checks if the session is marked as authenticated and has not exceeded
// the absolute session timeout.
//
// Returns:
//   - true if the "authenticated" flag is set to true and the session creation time is within the allowed timeout.
//   - false otherwise.
func (sd *SessionData) GetAuthenticated() bool {
	auth, _ := sd.mainSession.Values["authenticated"].(bool)
	if !auth {
		return false
	}

	// Check session expiration.
	createdAt, ok := sd.mainSession.Values["created_at"].(int64)
	if !ok {
		return false
	}
	return time.Since(time.Unix(createdAt, 0)) <= absoluteSessionTimeout
}

// SetAuthenticated sets the authentication status of the session.
// If setting to true, it generates a new secure session ID for the main session
// to prevent session fixation attacks and records the current time as the creation time.
//
// Parameters:
//   - value: The boolean authentication status (true for authenticated, false otherwise).
//
// Returns:
//   - An error if generating a new session ID fails when setting value to true.
func (sd *SessionData) SetAuthenticated(value bool) error {
	if value {
		id, err := generateSecureRandomString(32)
		if err != nil {
			return fmt.Errorf("failed to generate secure session id: %w", err)
		}
		sd.mainSession.ID = id
		sd.mainSession.Values["created_at"] = time.Now().Unix()
	}
	sd.mainSession.Values["authenticated"] = value
	return nil
}

// GetAccessToken retrieves the access token stored in the session.
// It handles reassembling the token from multiple cookie chunks if necessary
// and decompresses it if it was stored compressed.
//
// Returns:
//   - The complete, decompressed access token string, or an empty string if not found.
func (sd *SessionData) GetAccessToken() string {
	token, _ := sd.accessSession.Values["token"].(string)
	if token != "" {
		compressed, _ := sd.accessSession.Values["compressed"].(bool)
		if compressed {
			return decompressToken(token)
		}
		return token
	}

	// Reassemble token from chunks.
	if len(sd.accessTokenChunks) == 0 {
		return ""
	}

	var chunks []string
	for i := 0; ; i++ {
		session, ok := sd.accessTokenChunks[i]
		if !ok {
			break
		}
		chunk, _ := session.Values["token_chunk"].(string)
		chunks = append(chunks, chunk)
	}

	token = strings.Join(chunks, "")
	compressed, _ := sd.accessSession.Values["compressed"].(bool)
	if compressed {
		return decompressToken(token)
	}
	return token
}

// SetAccessToken stores the provided access token in the session.
// It first expires any existing access token chunk cookies.
// It then compresses the token. If the compressed token fits within a single cookie (maxCookieSize),
// it's stored directly in the primary access token session. Otherwise, the compressed token
// is split into chunks, and each chunk is stored in a separate numbered cookie (_oidc_raczylo_a_0, _oidc_raczylo_a_1, etc.).
//
// Parameters:
//   - token: The access token string to store.
func (sd *SessionData) SetAccessToken(token string) {
	// Expire any existing chunk cookies first.
	if sd.request != nil {
		sd.expireAccessTokenChunks(nil) // Will be saved when Save() is called.
	}

	// Clear and prepare chunks map for new token.
	sd.accessTokenChunks = make(map[int]*sessions.Session)

	// Compress token.
	compressed := compressToken(token)

	if len(compressed) <= maxCookieSize {
		sd.accessSession.Values["token"] = compressed
		sd.accessSession.Values["compressed"] = true
	} else {
		// Split compressed token into chunks.
		sd.accessSession.Values["token"] = ""
		sd.accessSession.Values["compressed"] = true
		chunks := splitIntoChunks(compressed, maxCookieSize)
		for i, chunk := range chunks {
			sessionName := fmt.Sprintf("%s_%d", accessTokenCookie, i)
			session, _ := sd.manager.store.Get(sd.request, sessionName)
			session.Values["token_chunk"] = chunk
			sd.accessTokenChunks[i] = session
		}
	}
}

// GetRefreshToken retrieves the refresh token stored in the session.
// It handles reassembling the token from multiple cookie chunks if necessary
// and decompresses it if it was stored compressed.
//
// Returns:
//   - The complete, decompressed refresh token string, or an empty string if not found.
func (sd *SessionData) GetRefreshToken() string {
	token, _ := sd.refreshSession.Values["token"].(string)
	if token != "" {
		compressed, _ := sd.refreshSession.Values["compressed"].(bool)
		if compressed {
			return decompressToken(token)
		}
		return token
	}

	// Reassemble token from chunks.
	if len(sd.refreshTokenChunks) == 0 {
		return ""
	}

	var chunks []string
	for i := 0; ; i++ {
		session, ok := sd.refreshTokenChunks[i]
		if !ok {
			break
		}
		chunk, _ := session.Values["token_chunk"].(string)
		chunks = append(chunks, chunk)
	}

	token = strings.Join(chunks, "")
	compressed, _ := sd.refreshSession.Values["compressed"].(bool)
	if compressed {
		return decompressToken(token)
	}
	return token
}

// SetRefreshToken stores the provided refresh token in the session.
// It first expires any existing refresh token chunk cookies.
// It then compresses the token. If the compressed token fits within a single cookie (maxCookieSize),
// it's stored directly in the primary refresh token session. Otherwise, the compressed token
// is split into chunks, and each chunk is stored in a separate numbered cookie (_oidc_raczylo_r_0, _oidc_raczylo_r_1, etc.).
//
// Parameters:
//   - token: The refresh token string to store.
func (sd *SessionData) SetRefreshToken(token string) {
	// Expire any existing chunk cookies first.
	if sd.request != nil {
		sd.expireRefreshTokenChunks(nil) // Will be saved when Save() is called.
	}

	// Clear and prepare chunks map for new token.
	sd.refreshTokenChunks = make(map[int]*sessions.Session)

	// Compress token.
	compressed := compressToken(token)

	if len(compressed) <= maxCookieSize {
		sd.refreshSession.Values["token"] = compressed
		sd.refreshSession.Values["compressed"] = true
	} else {
		// Split compressed token into chunks.
		sd.refreshSession.Values["token"] = ""
		sd.refreshSession.Values["compressed"] = true
		chunks := splitIntoChunks(compressed, maxCookieSize)
		for i, chunk := range chunks {
			sessionName := fmt.Sprintf("%s_%d", refreshTokenCookie, i)
			session, _ := sd.manager.store.Get(sd.request, sessionName)
			session.Values["token_chunk"] = chunk
			sd.refreshTokenChunks[i] = session
		}
	}
}

// expireAccessTokenChunks finds all existing access token chunk cookies (_oidc_raczylo_a_N)
// associated with the current request, clears their values, and sets their MaxAge to -1.
// If a ResponseWriter is provided, it attempts to save the expired chunk sessions to send
// the expiring Set-Cookie headers. This is used internally when setting a new access token.
//
// Parameters:
//   - w: The HTTP response writer (optional). If provided, expiring Set-Cookie headers will be sent.
func (sd *SessionData) expireAccessTokenChunks(w http.ResponseWriter) {
	for i := 0; ; i++ {
		sessionName := fmt.Sprintf("%s_%d", accessTokenCookie, i)
		session, err := sd.manager.store.Get(sd.request, sessionName)
		if err != nil || session.IsNew {
			break
		}
		session.Options.MaxAge = -1
		session.Values = make(map[interface{}]interface{})
		if w != nil {
			if err := session.Save(sd.request, w); err != nil {
				sd.manager.logger.Errorf("failed to save expired access token cookie: %v", err)
			}
		}
	}
}

// expireRefreshTokenChunks finds all existing refresh token chunk cookies (_oidc_raczylo_r_N)
// associated with the current request, clears their values, and sets their MaxAge to -1.
// If a ResponseWriter is provided, it attempts to save the expired chunk sessions to send
// the expiring Set-Cookie headers. This is used internally when setting a new refresh token.
//
// Parameters:
//   - w: The HTTP response writer (optional). If provided, expiring Set-Cookie headers will be sent.
func (sd *SessionData) expireRefreshTokenChunks(w http.ResponseWriter) {
	for i := 0; ; i++ {
		sessionName := fmt.Sprintf("%s_%d", refreshTokenCookie, i)
		session, err := sd.manager.store.Get(sd.request, sessionName)
		if err != nil || session.IsNew {
			break
		}
		session.Options.MaxAge = -1
		session.Values = make(map[interface{}]interface{})
		if w != nil {
			if err := session.Save(sd.request, w); err != nil {
				sd.manager.logger.Errorf("failed to save expired refresh token cookie: %v", err)
			}
		}
	}
}

// splitIntoChunks divides a string `s` into a slice of strings, where each element
// has a maximum length of `chunkSize`.
//
// Parameters:
//   - s: The string to split.
//   - chunkSize: The maximum size of each chunk.
//
// Returns:
//   - A slice of strings representing the chunks.
func splitIntoChunks(s string, chunkSize int) []string {
	var chunks []string
	for len(s) > 0 {
		if len(s) > chunkSize {
			chunks = append(chunks, s[:chunkSize])
			s = s[chunkSize:]
		} else {
			chunks = append(chunks, s)
			break
		}
	}
	return chunks
}

// GetCSRF retrieves the Cross-Site Request Forgery (CSRF) token stored in the main session.
//
// Returns:
//   - The CSRF token string, or an empty string if not set.
func (sd *SessionData) GetCSRF() string {
	csrf, _ := sd.mainSession.Values["csrf"].(string)
	return csrf
}

// SetCSRF stores the provided CSRF token string in the main session.
// This token is typically generated at the start of the authentication flow.
//
// Parameters:
//   - token: The CSRF token to store.
func (sd *SessionData) SetCSRF(token string) {
	sd.mainSession.Values["csrf"] = token
}

// GetNonce retrieves the OIDC nonce value stored in the main session.
// The nonce is used to associate an ID token with the specific authentication request.
//
// Returns:
//   - The nonce string, or an empty string if not set.
func (sd *SessionData) GetNonce() string {
	nonce, _ := sd.mainSession.Values["nonce"].(string)
	return nonce
}

// SetNonce stores the provided OIDC nonce string in the main session.
// This nonce is typically generated at the start of the authentication flow.
//
// Parameters:
//   - nonce: The nonce string to store.
func (sd *SessionData) SetNonce(nonce string) {
	sd.mainSession.Values["nonce"] = nonce
}

// GetCodeVerifier retrieves the PKCE (Proof Key for Code Exchange) code verifier
// stored in the main session. This is only relevant if PKCE is enabled.
//
// Returns:
//   - The code verifier string, or an empty string if not set or PKCE is disabled.
func (sd *SessionData) GetCodeVerifier() string {
	codeVerifier, _ := sd.mainSession.Values["code_verifier"].(string)
	return codeVerifier
}

// SetCodeVerifier stores the provided PKCE code verifier string in the main session.
// This is typically called at the start of the authentication flow if PKCE is enabled.
//
// Parameters:
//   - codeVerifier: The PKCE code verifier string to store.
func (sd *SessionData) SetCodeVerifier(codeVerifier string) {
	sd.mainSession.Values["code_verifier"] = codeVerifier
}

// GetEmail retrieves the authenticated user's email address stored in the main session.
// This is typically extracted from the ID token claims after successful authentication.
//
// Returns:
//   - The user's email address string, or an empty string if not set.
func (sd *SessionData) GetEmail() string {
	email, _ := sd.mainSession.Values["email"].(string)
	return email
}

// SetEmail stores the provided user email address string in the main session.
// This is typically called after successful authentication and claim extraction.
//
// Parameters:
//   - email: The user's email address to store.
func (sd *SessionData) SetEmail(email string) {
	sd.mainSession.Values["email"] = email
}

// GetIncomingPath retrieves the original request URI (including query parameters)
// that the user was trying to access before being redirected for authentication.
// This is stored in the main session to allow redirection back after successful login.
//
// Returns:
//   - The original request URI string, or an empty string if not set.
func (sd *SessionData) GetIncomingPath() string {
	path, _ := sd.mainSession.Values["incoming_path"].(string)
	return path
}

// SetIncomingPath stores the original request URI (path and query parameters)
// in the main session. This is typically called at the start of the authentication flow.
//
// Parameters:
//   - path: The original request URI string (e.g., "/protected/resource?id=123").
func (sd *SessionData) SetIncomingPath(path string) {
	sd.mainSession.Values["incoming_path"] = path
}
