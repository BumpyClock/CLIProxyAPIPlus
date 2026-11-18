package management

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/copilot"
	cursorauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/cursor"
	geminiAuth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/gemini"
	gitlabauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/gitlab"
	iflowauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/iflow"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kilo"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kiro"
	qoderauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/qoder"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/diff"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	geminiCLIEndpoint    = "https://cloudcode-pa.googleapis.com"
	geminiCLIVersion     = "v1internal"
	gitLabLoginModeOAuth = "oauth"
	gitLabLoginModePAT   = "pat"
	kiroCallbackPort     = 9876
)

var plusOAuthStarts = map[string]func(*Handler, *gin.Context){
	"cursor":         (*Handler).RequestCursorToken,
	"github-copilot": (*Handler).RequestGitHubToken,
	"gitlab":         (*Handler).RequestGitLabToken,
	"iflow":          (*Handler).RequestIFlowToken,
	"kilo":           (*Handler).RequestKiloToken,
	"kiro":           (*Handler).RequestKiroToken,
	"qoder":          (*Handler).RequestQoderToken,
}

func (h *Handler) ListPlusOAuthProvidersV8(c *gin.Context) {
	providers := slices.Sorted(maps.Keys(plusOAuthStarts))
	c.JSON(http.StatusOK, gin.H{"providers": providers})
}

func (h *Handler) startPlusOAuthV8(c *gin.Context, provider string) bool {
	start, ok := plusOAuthStarts[strings.ToLower(provider)]
	if !ok {
		return false
	}
	if _, targeted := c.Get(oauthSessionTargetContextKey); targeted || oauthSessionProxyURLFromContext(c) != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "targeted recovery and proxy_url are not supported for this provider"})
		return true
	}
	start(h, c)
	return true
}

func newPlusOAuthState(c *gin.Context) (string, bool) {
	state, err := misc.GenerateRandomState()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return "", false
	}
	return state, true
}

func startPlusDeviceOAuth(ctx context.Context, state, provider string, poll func(context.Context)) {
	oauthSessions.registerBuiltin(state, provider, "", oauthFlowDevice)
	go func() {
		pollCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		go watchOAuthSessionCancel(pollCtx, cancel, state, provider)
		poll(pollCtx)
	}()
}

func (h *Handler) beginPlusBrowserOAuth(c *gin.Context, state, provider string, callbackPort int) (func(), bool) {
	var forwarder *callbackForwarder
	if isWebUIRequest(c) {
		path := "/" + provider + "/callback"
		if c.GetBool(ConfigV8ContextKey) {
			path = "/v8/management/oauth/callback"
		}
		target, errTarget := h.managementCallbackURL(path)
		if errTarget != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
			return nil, false
		}
		var errListen error
		forwarder, errListen = listenCallbackForwarder(callbackPort, provider, target)
		if errListen != nil {
			log.WithError(errListen).Warn("OAuth callback port unavailable")
			c.JSON(http.StatusConflict, gin.H{"error": "callback port unavailable"})
			return nil, false
		}
	}
	oauthSessions.registerBuiltin(state, provider, "", oauthFlowBrowser)
	return func() { stopCallbackForwarderInstance(callbackPort, forwarder) }, true
}

func kiroDevicePrompt(response *kiroauth.StartDeviceAuthResponse) oauthPrompt {
	return oauthPrompt{
		URL:      firstNonEmpty(response.VerificationURIComplete, response.VerificationURI),
		UserCode: response.UserCode,
	}
}

func waitPlusOAuthCallback(authDir, provider, state, timeoutMessage string) *oauthCallbackFilePayload {
	path := oauthCallbackFilePath(authDir, provider, state)
	defer func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.WithError(err).Warn("failed to remove OAuth callback")
		}
	}()
	deadline := time.Now().Add(5 * time.Minute)
	for IsOAuthSessionPending(state, provider) {
		if time.Now().After(deadline) {
			SetOAuthSessionError(state, timeoutMessage)
			return nil
		}
		if data, err := os.ReadFile(path); err == nil {
			var callback oauthCallbackFilePayload
			if err := json.Unmarshal(data, &callback); err != nil {
				SetOAuthSessionError(state, "Invalid OAuth callback")
				return nil
			}
			return &callback
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil
}

func (h *Handler) RequestCursorToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	label := strings.TrimSpace(c.Query("label"))
	log.Infof("Initializing Cursor authentication (label=%q)...", label)

	authParams, err := cursorauth.GenerateAuthParams()
	if err != nil {
		log.Errorf("Failed to generate Cursor auth params: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate auth params"})
		return
	}

	state, ok := newPlusOAuthState(c)
	if !ok {
		return
	}
	startPlusDeviceOAuth(ctx, state, "cursor", func(pollCtx context.Context) {
		log.Info("Waiting for Cursor authentication...")
		log.Infof("Open this URL in your browser: %s", authParams.LoginURL)

		tokens, errPoll := cursorauth.PollForAuth(pollCtx, authParams.UUID, authParams.Verifier)
		if errPoll != nil {
			SetOAuthSessionError(state, "Authentication failed: "+errPoll.Error())
			log.Errorf("Cursor authentication failed: %v", errPoll)
			return
		}

		metadata := map[string]any{
			"type":          "cursor",
			"access_token":  tokens.AccessToken,
			"refresh_token": tokens.RefreshToken,
			"timestamp":     time.Now().UnixMilli(),
		}

		// Extract expiry and account identity from JWT
		expiry := cursorauth.GetTokenExpiry(tokens.AccessToken)
		if !expiry.IsZero() {
			metadata["expires_at"] = expiry.Format(time.RFC3339)
		}

		// Auto-identify account from JWT sub claim for multi-account support
		sub := cursorauth.ParseJWTSub(tokens.AccessToken)
		subHash := cursorauth.SubToShortHash(sub)
		if sub != "" {
			metadata["sub"] = sub
		}

		fileName := cursorauth.CredentialFileName(label, subHash)
		displayLabel := cursorauth.DisplayLabel(label, subHash)
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "cursor",
			FileName: fileName,
			Label:    displayLabel,
			Metadata: metadata,
		}
		savedPath, saved := h.commitAndCompleteBuiltinOAuthSession(ctx, state, "cursor", "", record, "Failed to save authentication tokens", false)
		if !saved {
			return
		}

		log.Infof("Cursor authentication successful! Token saved to %s", savedPath)

	})

	c.JSON(200, gin.H{
		"status": "ok",
		"url":    authParams.LoginURL,
		"state":  state,
	})
}
func (h *Handler) RequestGitHubToken(c *gin.Context) {
	ctx := context.Background()

	fmt.Println("Initializing GitHub Copilot authentication...")

	state, ok := newPlusOAuthState(c)
	if !ok {
		return
	}

	deviceClient := copilot.NewDeviceFlowClient(h.cfg)

	deviceCode, err := deviceClient.RequestDeviceCode(ctx)
	if err != nil {
		log.Errorf("Failed to initiate device flow: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to initiate device flow"})
		return
	}

	authURL := deviceCode.VerificationURI
	userCode := deviceCode.UserCode

	startPlusDeviceOAuth(ctx, state, "github-copilot", func(pollCtx context.Context) {
		fmt.Printf("Please visit %s and enter code: %s\n", authURL, userCode)

		tokenData, errPoll := deviceClient.PollForToken(pollCtx, deviceCode)
		if errPoll != nil {
			SetOAuthSessionError(state, "Authentication failed")
			fmt.Printf("Authentication failed: %v\n", errPoll)
			return
		}

		userInfo, errUser := deviceClient.FetchUserInfo(pollCtx, tokenData.AccessToken)
		if errUser != nil {
			log.Warnf("Failed to fetch user info: %v", errUser)
		}

		username := userInfo.Login
		if username == "" {
			username = "github-user"
		}

		tokenStorage := &copilot.CopilotTokenStorage{
			AccessToken: tokenData.AccessToken,
			TokenType:   tokenData.TokenType,
			Scope:       tokenData.Scope,
			Username:    username,
			Email:       userInfo.Email,
			Name:        userInfo.Name,
			Type:        "github-copilot",
		}

		fileName := fmt.Sprintf("github-copilot-%s.json", username)
		label := userInfo.Email
		if label == "" {
			label = username
		}
		metadata, errMeta := copilotTokenMetadata(tokenStorage)
		if errMeta != nil {
			log.Errorf("Failed to build token metadata: %v", errMeta)
			SetOAuthSessionError(state, "Failed to build token metadata")
			return
		}

		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "github-copilot",
			Label:    label,
			FileName: fileName,
			Storage:  tokenStorage,
			Metadata: metadata,
		}

		savedPath, saved := h.commitAndCompleteBuiltinOAuthSession(ctx, state, "github-copilot", "", record, "Failed to save authentication tokens", false)
		if !saved {
			return
		}

		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		fmt.Println("You can now use GitHub Copilot services through this CLI")

	})

	c.JSON(200, gin.H{
		"status":           "ok",
		"url":              authURL,
		"state":            state,
		"user_code":        userCode,
		"verification_uri": authURL,
	})
}

func (h *Handler) RequestGitLabPATToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	var payload struct {
		BaseURL             string `json:"base_url"`
		PersonalAccessToken string `json:"personal_access_token"`
		Token               string `json:"token"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid body"})
		return
	}

	baseURL := gitlabauth.NormalizeBaseURL(payload.BaseURL)
	if c.GetBool(ConfigV8ContextKey) {
		if strings.TrimSpace(payload.BaseURL) == "" {
			baseURL = gitLabBaseURLFromRequest(nil)
		}
		if errURL := validateGitLabBaseURL(baseURL); errURL != nil {
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": errURL.Error()})
			return
		}
	}
	pat := strings.TrimSpace(payload.PersonalAccessToken)
	if pat == "" {
		pat = strings.TrimSpace(payload.Token)
	}
	if pat == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "personal_access_token is required"})
		return
	}

	authClient := h.gitLabManagementClient(c)

	user, err := authClient.GetCurrentUser(ctx, baseURL, pat)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": err.Error()})
		return
	}
	patSelf, err := authClient.GetPersonalAccessTokenSelf(ctx, baseURL, pat)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": err.Error()})
		return
	}
	direct, err := authClient.FetchDirectAccess(ctx, baseURL, pat)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": err.Error()})
		return
	}

	identifier := gitLabAccountIdentifier(user)
	fileName := fmt.Sprintf("gitlab-%s-pat.json", sanitizeGitLabFileName(identifier))
	metadata := buildGitLabAuthMetadata(baseURL, gitLabLoginModePAT, nil, direct)
	metadata["auth_kind"] = "personal_access_token"
	metadata["personal_access_token"] = pat
	metadata["token_preview"] = maskGitLabToken(pat)
	metadata["username"] = strings.TrimSpace(user.Username)
	if email := primaryGitLabEmail(user); email != "" {
		metadata["email"] = email
	}
	metadata["name"] = strings.TrimSpace(user.Name)
	if patSelf != nil {
		if name := strings.TrimSpace(patSelf.Name); name != "" {
			metadata["pat_name"] = name
		}
		if len(patSelf.Scopes) > 0 {
			metadata["pat_scopes"] = append([]string(nil), patSelf.Scopes...)
		}
	}

	record := &coreauth.Auth{
		ID:       fileName,
		Provider: "gitlab",
		FileName: fileName,
		Label:    identifier + " (PAT)",
		Metadata: metadata,
	}

	savedPath, err := h.saveTokenRecord(ctx, record)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "failed to save authentication tokens"})
		return
	}

	response := gin.H{
		"status":      "ok",
		"saved_path":  savedPath,
		"username":    strings.TrimSpace(user.Username),
		"email":       primaryGitLabEmail(user),
		"token_label": identifier,
	}
	if direct != nil && direct.ModelDetails != nil {
		if provider := strings.TrimSpace(direct.ModelDetails.ModelProvider); provider != "" {
			response["model_provider"] = provider
		}
		if model := strings.TrimSpace(direct.ModelDetails.ModelName); model != "" {
			response["model_name"] = model
		}
	}

	fmt.Printf("GitLab Duo PAT authentication successful. Token saved to %s\n", savedPath)
	c.JSON(http.StatusOK, response)
}

func (h *Handler) RequestGitLabToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	fmt.Println("Initializing GitLab Duo authentication...")

	baseURL, clientID, clientSecret, errSettings := gitLabOAuthClientSettings(c)
	if errSettings != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errSettings.Error()})
		return
	}

	pkceCodes, err := gitlabauth.GeneratePKCECodes()
	if err != nil {
		log.Errorf("Failed to generate GitLab PKCE codes: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate PKCE codes"})
		return
	}

	state, ok := newPlusOAuthState(c)
	if !ok {
		return
	}

	redirectURI := gitlabauth.RedirectURL(gitlabauth.DefaultCallbackPort)
	authClient := h.gitLabManagementClient(c)
	authURL, err := authClient.GenerateAuthURL(baseURL, clientID, redirectURI, state, pkceCodes)
	if err != nil {
		log.Errorf("Failed to generate GitLab authorization URL: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}

	closeBrowser, ok := h.beginPlusBrowserOAuth(c, state, "gitlab", gitlabauth.DefaultCallbackPort)
	if !ok {
		return
	}

	go func() {
		defer closeBrowser()

		callback := waitPlusOAuthCallback(h.cfg.AuthDir, "gitlab", state, "Timeout waiting for OAuth callback")
		if callback == nil {
			return
		}
		if errStr := strings.TrimSpace(callback.Error); errStr != "" {
			SetOAuthSessionError(state, errStr)
			return
		}
		if strings.TrimSpace(callback.State) != state {
			SetOAuthSessionError(state, "State code error")
			return
		}
		code := strings.TrimSpace(callback.Code)
		if code == "" {
			SetOAuthSessionError(state, "Authorization code missing")
			return
		}

		tokenResp, errExchange := authClient.ExchangeCodeForTokens(ctx, baseURL, clientID, clientSecret, redirectURI, code, pkceCodes.CodeVerifier)
		if errExchange != nil {
			log.Errorf("Failed to exchange GitLab authorization code: %v", errExchange)
			SetOAuthSessionError(state, "Failed to exchange authorization code for tokens")
			return
		}

		user, errUser := authClient.GetCurrentUser(ctx, baseURL, tokenResp.AccessToken)
		if errUser != nil {
			log.Errorf("Failed to fetch GitLab user profile: %v", errUser)
			SetOAuthSessionError(state, "Failed to fetch account profile")
			return
		}

		direct, errDirect := authClient.FetchDirectAccess(ctx, baseURL, tokenResp.AccessToken)
		if errDirect != nil {
			log.Errorf("Failed to fetch GitLab direct access metadata: %v", errDirect)
			SetOAuthSessionError(state, "Failed to fetch GitLab Duo access")
			return
		}

		identifier := gitLabAccountIdentifier(user)
		fileName := fmt.Sprintf("gitlab-%s.json", sanitizeGitLabFileName(identifier))
		metadata := buildGitLabAuthMetadata(baseURL, gitLabLoginModeOAuth, tokenResp, direct)
		metadata["auth_kind"] = "oauth"
		metadata["oauth_client_id"] = clientID
		metadata["username"] = strings.TrimSpace(user.Username)
		if email := primaryGitLabEmail(user); email != "" {
			metadata["email"] = email
		}
		metadata["name"] = strings.TrimSpace(user.Name)

		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "gitlab",
			FileName: fileName,
			Label:    identifier,
			Metadata: metadata,
		}
		savedPath, saved := h.commitAndCompleteBuiltinOAuthSession(ctx, state, "gitlab", "", record, "Failed to save authentication tokens", false)
		if !saved {
			return
		}

		fmt.Printf("GitLab Duo authentication successful. Token saved to %s\n", savedPath)

	}()

	c.JSON(http.StatusOK, gin.H{"status": "ok", "url": authURL, "state": state})
}

func gitLabOAuthClientSettings(c *gin.Context) (baseURL, clientID, clientSecret string, err error) {
	configuredBase := gitLabBaseURLFromRequest(nil)
	configuredID := strings.TrimSpace(os.Getenv("GITLAB_OAUTH_CLIENT_ID"))
	baseURL = gitLabBaseURLFromRequest(c)
	clientID = strings.TrimSpace(c.Query("client_id"))
	clientSecret = strings.TrimSpace(c.Query("client_secret"))
	if c.GetBool(ConfigV8ContextKey) {
		if errURL := validateGitLabBaseURL(baseURL); errURL != nil {
			return "", "", "", errURL
		}
		if _, inQuery := c.GetQuery("client_secret"); inQuery {
			return "", "", "", fmt.Errorf("client_secret must be provided in the request body")
		}
		metadata := oauthSessionExtraMetadataFromContext(c)
		if v, ok := metadata["client_secret"]; ok {
			secret, isString := v.(string)
			if !isString {
				return "", "", "", fmt.Errorf("invalid client_secret")
			}
			clientSecret = strings.TrimSpace(secret)
		}
	}
	if baseURL == configuredBase {
		if clientID == "" {
			clientID = configuredID
		}
		if clientSecret == "" && clientID == configuredID {
			clientSecret = strings.TrimSpace(os.Getenv("GITLAB_OAUTH_CLIENT_SECRET"))
		}
	}
	if clientID == "" {
		return "", "", "", fmt.Errorf("gitlab client_id is required")
	}
	return baseURL, clientID, clientSecret, nil
}

func validateGitLabBaseURL(raw string) error {
	destination, err := url.Parse(raw)
	if err != nil || destination.Hostname() == "" || destination.User != nil || destination.RawQuery != "" || destination.Fragment != "" {
		return fmt.Errorf("invalid gitlab base_url")
	}
	host := destination.Hostname()
	loopback := strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
	if destination.Scheme != "https" && !(destination.Scheme == "http" && loopback) {
		return fmt.Errorf("gitlab base_url must use HTTPS outside loopback")
	}
	return nil
}

func (h *Handler) gitLabManagementClient(c *gin.Context) *gitlabauth.AuthClient {
	if c.GetBool(ConfigV8ContextKey) {
		return gitlabauth.NewAuthClientWithoutRedirects(h.cfg)
	}
	return gitlabauth.NewAuthClient(h.cfg)
}

func (h *Handler) RequestIFlowCookieToken(c *gin.Context) {
	ctx := context.Background()

	var payload struct {
		Cookie string `json:"cookie"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "cookie is required"})
		return
	}

	cookieValue := strings.TrimSpace(payload.Cookie)

	if cookieValue == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "cookie is required"})
		return
	}

	cookieValue, errNormalize := iflowauth.NormalizeCookie(cookieValue)
	if errNormalize != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": errNormalize.Error()})
		return
	}

	// Check for duplicate BXAuth before authentication
	bxAuth := iflowauth.ExtractBXAuth(cookieValue)
	if existingFile, err := iflowauth.CheckDuplicateBXAuth(h.cfg.AuthDir, bxAuth); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "failed to check duplicate"})
		return
	} else if existingFile != "" {
		existingFileName := filepath.Base(existingFile)
		c.JSON(http.StatusConflict, gin.H{"status": "error", "error": "duplicate BXAuth found", "existing_file": existingFileName})
		return
	}

	authSvc := iflowauth.NewIFlowAuth(h.cfg)
	tokenData, errAuth := authSvc.AuthenticateWithCookie(ctx, cookieValue)
	if errAuth != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": errAuth.Error()})
		return
	}

	tokenData.Cookie = cookieValue

	tokenStorage := authSvc.CreateCookieTokenStorage(tokenData)
	email := strings.TrimSpace(tokenStorage.Email)
	if email == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "failed to extract email from token"})
		return
	}

	fileName := iflowauth.SanitizeIFlowFileName(email)
	if fileName == "" {
		fileName = fmt.Sprintf("iflow-%d", time.Now().UnixMilli())
	} else {
		fileName = fmt.Sprintf("iflow-%s", fileName)
	}

	tokenStorage.Email = email
	timestamp := time.Now().Unix()

	record := &coreauth.Auth{
		ID:       fmt.Sprintf("%s-%d.json", fileName, timestamp),
		Provider: "iflow",
		FileName: fmt.Sprintf("%s-%d.json", fileName, timestamp),
		Storage:  tokenStorage,
		Metadata: map[string]any{
			"email":        email,
			"api_key":      tokenStorage.APIKey,
			"expired":      tokenStorage.Expire,
			"cookie":       tokenStorage.Cookie,
			"type":         tokenStorage.Type,
			"last_refresh": tokenStorage.LastRefresh,
		},
		Attributes: map[string]string{
			"api_key": tokenStorage.APIKey,
		},
	}

	savedPath, errSave := h.saveTokenRecord(ctx, record)
	if errSave != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "failed to save authentication tokens"})
		return
	}

	fmt.Printf("iFlow cookie authentication successful. Token saved to %s\n", savedPath)
	c.JSON(http.StatusOK, gin.H{
		"status":     "ok",
		"saved_path": savedPath,
		"email":      email,
		"expired":    tokenStorage.Expire,
		"type":       tokenStorage.Type,
	})
}

type projectSelectionRequiredError struct{}

func (e *projectSelectionRequiredError) Error() string {
	return "gemini cli: project selection required"
}

func (h *Handler) RequestIFlowToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	fmt.Println("Initializing iFlow authentication...")

	state, ok := newPlusOAuthState(c)
	if !ok {
		return
	}
	authSvc := iflowauth.NewIFlowAuth(h.cfg)
	authURL, redirectURI := authSvc.AuthorizationURL(state, iflowauth.CallbackPort)

	closeBrowser, ok := h.beginPlusBrowserOAuth(c, state, "iflow", iflowauth.CallbackPort)
	if !ok {
		return
	}

	go func() {
		defer closeBrowser()
		fmt.Println("Waiting for authentication...")

		callback := waitPlusOAuthCallback(h.cfg.AuthDir, "iflow", state, "Authentication failed")
		if callback == nil {
			return
		}
		if errStr := strings.TrimSpace(callback.Error); errStr != "" {
			SetOAuthSessionError(state, "Authentication failed")
			fmt.Printf("Authentication failed: %s\n", errStr)
			return
		}
		if resultState := strings.TrimSpace(callback.State); resultState != state {
			SetOAuthSessionError(state, "Authentication failed")
			fmt.Println("Authentication failed: state mismatch")
			return
		}

		code := strings.TrimSpace(callback.Code)
		if code == "" {
			SetOAuthSessionError(state, "Authentication failed")
			fmt.Println("Authentication failed: code missing")
			return
		}

		tokenData, errExchange := authSvc.ExchangeCodeForTokens(ctx, code, redirectURI)
		if errExchange != nil {
			SetOAuthSessionError(state, "Authentication failed")
			fmt.Printf("Authentication failed: %v\n", errExchange)
			return
		}

		tokenStorage := authSvc.CreateTokenStorage(tokenData)
		identifier := strings.TrimSpace(tokenStorage.Email)
		if identifier == "" {
			identifier = fmt.Sprintf("%d", time.Now().UnixMilli())
			tokenStorage.Email = identifier
		}
		record := &coreauth.Auth{
			ID:         fmt.Sprintf("iflow-%s.json", identifier),
			Provider:   "iflow",
			FileName:   fmt.Sprintf("iflow-%s.json", identifier),
			Storage:    tokenStorage,
			Metadata:   map[string]any{"email": identifier, "api_key": tokenStorage.APIKey},
			Attributes: map[string]string{"api_key": tokenStorage.APIKey},
		}

		savedPath, saved := h.commitAndCompleteBuiltinOAuthSession(ctx, state, "iflow", "", record, "Failed to save authentication tokens", false)
		if !saved {
			return
		}

		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		if tokenStorage.APIKey != "" {
			fmt.Println("API key obtained and saved")
		}
		fmt.Println("You can now use iFlow services through this CLI")

	}()

	c.JSON(http.StatusOK, gin.H{"status": "ok", "url": authURL, "state": state})
}

func (h *Handler) RequestKiloToken(c *gin.Context) {
	ctx := context.Background()

	fmt.Println("Initializing Kilo authentication...")

	state, ok := newPlusOAuthState(c)
	if !ok {
		return
	}
	kilocodeAuth := kilo.NewKiloAuth()

	resp, err := kilocodeAuth.InitiateDeviceFlow(ctx)
	if err != nil {
		log.Errorf("Failed to initiate device flow: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to initiate device flow"})
		return
	}

	startPlusDeviceOAuth(ctx, state, "kilo", func(pollCtx context.Context) {
		fmt.Printf("Please visit %s and enter code: %s\n", resp.VerificationURL, resp.Code)

		status, err := kilocodeAuth.PollForToken(pollCtx, resp.Code)
		if err != nil {
			SetOAuthSessionError(state, "Authentication failed")
			fmt.Printf("Authentication failed: %v\n", err)
			return
		}

		profile, err := kilocodeAuth.GetProfile(pollCtx, status.Token)
		if err != nil {
			log.Warnf("Failed to fetch profile: %v", err)
			profile = &kilo.Profile{Email: status.UserEmail}
		}

		var orgID string
		if len(profile.Orgs) > 0 {
			orgID = profile.Orgs[0].ID
		}

		defaults, err := kilocodeAuth.GetDefaults(ctx, status.Token, orgID)
		if err != nil {
			defaults = &kilo.Defaults{}
		}

		ts := &kilo.KiloTokenStorage{
			Token:          status.Token,
			OrganizationID: orgID,
			Model:          defaults.Model,
			Email:          status.UserEmail,
			Type:           "kilo",
		}

		fileName := kilo.CredentialFileName(status.UserEmail)
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "kilo",
			FileName: fileName,
			Storage:  ts,
			Metadata: map[string]any{
				"email":           status.UserEmail,
				"organization_id": orgID,
				"model":           defaults.Model,
			},
		}

		savedPath, saved := h.commitAndCompleteBuiltinOAuthSession(ctx, state, "kilo", "", record, "Failed to save authentication tokens", false)
		if !saved {
			return
		}

		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)

	})

	c.JSON(200, gin.H{
		"status":           "ok",
		"url":              resp.VerificationURL,
		"state":            state,
		"user_code":        resp.Code,
		"verification_uri": resp.VerificationURL,
	})
}

func (h *Handler) RequestKiroToken(c *gin.Context) {
	ctx := context.Background()

	method := strings.ToLower(strings.TrimSpace(c.Query("method")))
	if method == "" {
		method = "aws"
	}

	fmt.Println("Initializing Kiro authentication...")

	state, ok := newPlusOAuthState(c)
	if !ok {
		return
	}

	switch method {
	case "aws", "builder-id":
		startPlusDeviceOAuth(ctx, state, "kiro", func(pollCtx context.Context) {
			ssoClient := kiroauth.NewSSOOIDCClient(h.cfg)

			fmt.Println("Registering client...")
			regResp, errRegister := ssoClient.RegisterClient(pollCtx)
			if errRegister != nil {
				log.Errorf("Failed to register client: %v", errRegister)
				SetOAuthSessionError(state, "Failed to register client")
				return
			}

			fmt.Println("Starting device authorization...")
			authResp, errAuth := ssoClient.StartDeviceAuthorization(pollCtx, regResp.ClientID, regResp.ClientSecret)
			if errAuth != nil {
				log.Errorf("Failed to start device auth: %v", errAuth)
				SetOAuthSessionError(state, "Failed to start device authorization")
				return
			}

			oauthSessions.setPrompt(state, kiroDevicePrompt(authResp))

			fmt.Println("Waiting for authorization...")
			interval := 5 * time.Second
			if authResp.Interval > 0 {
				interval = time.Duration(authResp.Interval) * time.Second
			}
			deadline := time.Now().Add(time.Duration(authResp.ExpiresIn) * time.Second)

			for time.Now().Before(deadline) {
				select {
				case <-pollCtx.Done():
					SetOAuthSessionError(state, "Authorization cancelled")
					return
				case <-time.After(interval):
					tokenResp, errToken := ssoClient.CreateToken(pollCtx, regResp.ClientID, regResp.ClientSecret, authResp.DeviceCode)
					if errToken != nil {
						errStr := errToken.Error()
						if strings.Contains(errStr, "authorization_pending") {
							continue
						}
						if strings.Contains(errStr, "slow_down") {
							interval += 5 * time.Second
							continue
						}
						log.Errorf("Token creation failed: %v", errToken)
						SetOAuthSessionError(state, "Token creation failed")
						return
					}

					// Success! Save the token
					expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
					email := kiroauth.ExtractEmailFromJWT(tokenResp.AccessToken)

					idPart := kiroauth.SanitizeEmailForFilename(email)
					if idPart == "" {
						idPart = fmt.Sprintf("%d", time.Now().UnixNano()%100000)
					}

					now := time.Now()
					fileName := fmt.Sprintf("kiro-aws-%s.json", idPart)

					record := &coreauth.Auth{
						ID:       fileName,
						Provider: "kiro",
						FileName: fileName,
						Metadata: map[string]any{
							"type":          "kiro",
							"access_token":  tokenResp.AccessToken,
							"refresh_token": tokenResp.RefreshToken,
							"expires_at":    expiresAt.Format(time.RFC3339),
							"auth_method":   "builder-id",
							"provider":      "AWS",
							"client_id":     regResp.ClientID,
							"client_secret": regResp.ClientSecret,
							"email":         email,
							"last_refresh":  now.Format(time.RFC3339),
						},
					}

					savedPath, saved := h.commitAndCompleteBuiltinOAuthSession(ctx, state, "kiro", "", record, "Failed to save authentication tokens", false)
					if !saved {
						return
					}

					fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
					if email != "" {
						fmt.Printf("Authenticated as: %s\n", email)
					}

					return
				}
			}

			SetOAuthSessionError(state, "Authorization timed out")
		})

		c.JSON(http.StatusOK, gin.H{"status": "ok", "state": state, "method": "device_code"})

	case "google", "github":
		// Social auth uses protocol handler - for WEB UI we use a callback forwarder
		provider := "Google"
		if method == "github" {
			provider = "Github"
		}

		closeBrowser, ok := h.beginPlusBrowserOAuth(c, state, "kiro", kiroCallbackPort)
		if !ok {
			return
		}

		go func() {
			defer closeBrowser()

			socialClient := kiroauth.NewSocialAuthClient(h.cfg)

			codeVerifier, codeChallenge, errPKCE := generateKiroPKCE()
			if errPKCE != nil {
				log.Errorf("Failed to generate PKCE: %v", errPKCE)
				SetOAuthSessionError(state, "Failed to generate PKCE")
				return
			}

			authURL := fmt.Sprintf("%s/login?idp=%s&redirect_uri=%s&code_challenge=%s&code_challenge_method=S256&state=%s&prompt=select_account",
				"https://prod.us-east-1.auth.desktop.kiro.dev",
				provider,
				url.QueryEscape(kiroauth.KiroRedirectURI),
				codeChallenge,
				state,
			)

			oauthSessions.setPrompt(state, oauthPrompt{URL: authURL})

			callback := waitPlusOAuthCallback(h.cfg.AuthDir, "kiro", state, "OAuth flow timed out")
			if callback == nil {
				return
			}
			if errStr := callback.Error; errStr != "" {
				log.Errorf("Authentication failed: %s", errStr)
				SetOAuthSessionError(state, "Authentication failed")
				return
			}
			if callback.State != state {
				log.Errorf("State mismatch")
				SetOAuthSessionError(state, "State mismatch")
				return
			}
			code := callback.Code
			if code == "" {
				log.Error("No authorization code received")
				SetOAuthSessionError(state, "No authorization code received")
				return
			}

			tokenReq := &kiroauth.CreateTokenRequest{
				Code:         code,
				CodeVerifier: codeVerifier,
				RedirectURI:  kiroauth.KiroRedirectURI,
			}

			tokenResp, errToken := socialClient.CreateToken(ctx, tokenReq)
			if errToken != nil {
				log.Errorf("Failed to exchange code for tokens: %v", errToken)
				SetOAuthSessionError(state, "Failed to exchange code for tokens")
				return
			}

			expiresIn := tokenResp.ExpiresIn
			if expiresIn <= 0 {
				expiresIn = 3600
			}
			expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second)
			email := kiroauth.ExtractEmailFromJWT(tokenResp.AccessToken)

			idPart := kiroauth.SanitizeEmailForFilename(email)
			if idPart == "" {
				idPart = fmt.Sprintf("%d", time.Now().UnixNano()%100000)
			}

			now := time.Now()
			fileName := fmt.Sprintf("kiro-%s-%s.json", strings.ToLower(provider), idPart)

			record := &coreauth.Auth{
				ID:       fileName,
				Provider: "kiro",
				FileName: fileName,
				Metadata: map[string]any{
					"type":          "kiro",
					"access_token":  tokenResp.AccessToken,
					"refresh_token": tokenResp.RefreshToken,
					"profile_arn":   tokenResp.ProfileArn,
					"expires_at":    expiresAt.Format(time.RFC3339),
					"auth_method":   "social",
					"provider":      provider,
					"email":         email,
					"last_refresh":  now.Format(time.RFC3339),
				},
			}

			savedPath, saved := h.commitAndCompleteBuiltinOAuthSession(ctx, state, "kiro", "", record, "Failed to save authentication tokens", false)
			if !saved {
				return
			}

			fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
			if email != "" {
				fmt.Printf("Authenticated as: %s\n", email)
			}
		}()

		c.JSON(http.StatusOK, gin.H{"status": "ok", "state": state, "method": "social"})

	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid method, use 'aws', 'google', or 'github'"})
	}
}

func (h *Handler) RequestQoderToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	fmt.Println("Initializing Qoder authentication...")

	state, ok := newPlusOAuthState(c)
	if !ok {
		return
	}
	qoderAuth := qoderauth.NewQoderAuth(h.cfg)

	deviceFlow, err := qoderAuth.InitiateDeviceFlow(ctx)
	if err != nil {
		log.Errorf("Failed to generate authorization URL: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}

	startPlusDeviceOAuth(ctx, state, "qoder", func(pollCtx context.Context) {
		fmt.Println("Waiting for authentication...")
		tokenData, errPollForToken := qoderAuth.PollForToken(pollCtx, deviceFlow)
		if errPollForToken != nil {
			SetOAuthSessionError(state, "Authentication failed")
			fmt.Printf("Authentication failed: %v\n", errPollForToken)
			return
		}

		storage := qoderAuth.CreateTokenStorage(tokenData, deviceFlow.MachineID)
		// Persist the organization identity as well as the display label. The
		// current Qoder CN gateway requires organization_id in COSY headers.
		name, email := "", ""
		if qoderAuth.IsEnterpriseVPC() {
			if details, errDetails := qoderAuth.FetchUserInfoDetails(pollCtx, tokenData.AccessToken); errDetails == nil {
				if storage.UserID == "" {
					storage.UserID = details.UserID
				}
				name = details.Name
				email = details.Email
				storage.OrganizationID = details.OrganizationID
				storage.OrganizationTags = details.OrganizationTags
			}
		} else {
			name, email = qoderAuth.SaveUserInfo(ctx, tokenData.AccessToken, tokenData.UserID, "", "")
		}
		storage.Name = name
		switch {
		case strings.TrimSpace(email) != "":
			storage.Email = strings.TrimSpace(email)
		case strings.TrimSpace(storage.UserID) != "":
			storage.Email = strings.TrimSpace(storage.UserID)
		default:
			storage.Email = fmt.Sprintf("user-%d", time.Now().UnixMilli())
		}
		fileName := fmt.Sprintf("qoder-%s.json", storage.Email)
		metadata := map[string]any{
			"email":   storage.Email,
			"name":    storage.Name,
			"user_id": storage.UserID,
		}
		if qoderAuth.IsEnterpriseVPC() {
			metadata["organization_id"] = storage.OrganizationID
			metadata["organization_tags"] = storage.OrganizationTags
		}
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "qoder",
			FileName: fileName,
			Label: func() string {
				if storage.Name != "" {
					return storage.Name
				}
				if storage.Email != "" {
					return storage.Email
				}
				return "Qoder User"
			}(),
			Storage:  storage,
			Metadata: metadata,
		}
		savedPath, saved := h.commitAndCompleteBuiltinOAuthSession(ctx, state, "qoder", "", record, "Failed to save authentication tokens", false)
		if !saved {
			return
		}

		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		fmt.Println("You can now use Qoder services through this CLI")

	})

	c.JSON(200, gin.H{"status": "ok", "url": deviceFlow.VerificationURIComplete, "state": state})
}

func authFileExcludedModelsValue(value any) []string {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if value, ok := item.(string); ok {
				out = append(out, value)
			}
		}
		return out
	case string:
		return strings.Split(typed, ",")
	default:
		return nil
	}
}

func buildGitLabAuthMetadata(baseURL, mode string, tokenResp *gitlabauth.TokenResponse, direct *gitlabauth.DirectAccessResponse) map[string]any {
	metadata := map[string]any{
		"type":                     "gitlab",
		"auth_method":              strings.TrimSpace(mode),
		"base_url":                 gitlabauth.NormalizeBaseURL(baseURL),
		"last_refresh":             time.Now().UTC().Format(time.RFC3339),
		"refresh_interval_seconds": 240,
	}
	if tokenResp != nil {
		metadata["access_token"] = strings.TrimSpace(tokenResp.AccessToken)
		if refreshToken := strings.TrimSpace(tokenResp.RefreshToken); refreshToken != "" {
			metadata["refresh_token"] = refreshToken
		}
		if tokenType := strings.TrimSpace(tokenResp.TokenType); tokenType != "" {
			metadata["token_type"] = tokenType
		}
		if scope := strings.TrimSpace(tokenResp.Scope); scope != "" {
			metadata["scope"] = scope
		}
		if expiry := gitlabauth.TokenExpiry(time.Now(), tokenResp); !expiry.IsZero() {
			metadata["oauth_expires_at"] = expiry.Format(time.RFC3339)
		}
	}
	mergeGitLabDirectAccessMetadata(metadata, direct)
	return metadata
}

func callGeminiCLI(ctx context.Context, httpClient *http.Client, endpoint string, body any, result any) error {
	endPointURL := fmt.Sprintf("%s/%s:%s", geminiCLIEndpoint, geminiCLIVersion, endpoint)
	if strings.HasPrefix(endpoint, "operations/") {
		endPointURL = fmt.Sprintf("%s/%s", geminiCLIEndpoint, endpoint)
	}

	var reader io.Reader
	if body != nil {
		rawBody, errMarshal := json.Marshal(body)
		if errMarshal != nil {
			return fmt.Errorf("marshal request body: %w", errMarshal)
		}
		reader = bytes.NewReader(rawBody)
	}

	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, endPointURL, reader)
	if errRequest != nil {
		return fmt.Errorf("create request: %w", errRequest)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", misc.GeminiCLIUserAgent(""))

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return fmt.Errorf("execute request: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("api request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	if result == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	if errDecode := json.NewDecoder(resp.Body).Decode(result); errDecode != nil {
		return fmt.Errorf("decode response body: %w", errDecode)
	}

	return nil
}

func checkCloudAPIIsEnabled(ctx context.Context, httpClient *http.Client, projectID string) (bool, error) {
	serviceUsageURL := "https://serviceusage.googleapis.com"
	requiredServices := []string{
		"cloudaicompanion.googleapis.com",
	}
	for _, service := range requiredServices {
		checkURL := fmt.Sprintf("%s/v1/projects/%s/services/%s", serviceUsageURL, projectID, service)
		req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, checkURL, nil)
		if errRequest != nil {
			return false, fmt.Errorf("failed to create request: %w", errRequest)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", misc.GeminiCLIUserAgent(""))
		resp, errDo := httpClient.Do(req)
		if errDo != nil {
			return false, fmt.Errorf("failed to execute request: %w", errDo)
		}

		if resp.StatusCode == http.StatusOK {
			bodyBytes, _ := io.ReadAll(resp.Body)
			if ojson.GetBytes(bodyBytes, "state").String() == "ENABLED" {
				_ = resp.Body.Close()
				continue
			}
		}
		_ = resp.Body.Close()

		enableURL := fmt.Sprintf("%s/v1/projects/%s/services/%s:enable", serviceUsageURL, projectID, service)
		req, errRequest = http.NewRequestWithContext(ctx, http.MethodPost, enableURL, strings.NewReader("{}"))
		if errRequest != nil {
			return false, fmt.Errorf("failed to create request: %w", errRequest)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", misc.GeminiCLIUserAgent(""))
		resp, errDo = httpClient.Do(req)
		if errDo != nil {
			return false, fmt.Errorf("failed to execute request: %w", errDo)
		}

		bodyBytes, _ := io.ReadAll(resp.Body)
		errMessage := string(bodyBytes)
		errMessageResult := ojson.GetBytes(bodyBytes, "error.message")
		if errMessageResult.Exists() {
			errMessage = errMessageResult.String()
		}
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
			_ = resp.Body.Close()
			continue
		} else if resp.StatusCode == http.StatusBadRequest {
			_ = resp.Body.Close()
			if strings.Contains(strings.ToLower(errMessage), "already enabled") {
				continue
			}
		}
		_ = resp.Body.Close()
		return false, fmt.Errorf("project activation required: %s", errMessage)
	}
	return true, nil
}

func copilotTokenMetadata(storage *copilot.CopilotTokenStorage) (map[string]any, error) {
	if storage == nil {
		return nil, fmt.Errorf("token storage is nil")
	}
	payload, errMarshal := json.Marshal(storage)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal token storage: %w", errMarshal)
	}
	metadata := make(map[string]any)
	if errUnmarshal := json.Unmarshal(payload, &metadata); errUnmarshal != nil {
		return nil, fmt.Errorf("unmarshal token storage: %w", errUnmarshal)
	}
	return metadata, nil
}

func ensureGeminiProjectAndOnboard(ctx context.Context, httpClient *http.Client, storage *geminiAuth.GeminiTokenStorage, requestedProject string) error {
	if storage == nil {
		return fmt.Errorf("gemini storage is nil")
	}

	trimmedRequest := strings.TrimSpace(requestedProject)
	if trimmedRequest == "" {
		projects, errProjects := fetchGCPProjects(ctx, httpClient)
		if errProjects != nil {
			return fmt.Errorf("fetch project list: %w", errProjects)
		}
		if len(projects) == 0 {
			return fmt.Errorf("no Google Cloud projects available for this account")
		}
		trimmedRequest = strings.TrimSpace(projects[0].ProjectID)
		if trimmedRequest == "" {
			return fmt.Errorf("resolved project id is empty")
		}
		storage.Auto = true
	} else {
		storage.Auto = false
	}

	if err := performGeminiCLISetup(ctx, httpClient, storage, trimmedRequest); err != nil {
		return err
	}

	if strings.TrimSpace(storage.ProjectID) == "" {
		storage.ProjectID = trimmedRequest
	}

	return nil
}

func ensureGeminiProjectsEnabled(ctx context.Context, httpClient *http.Client, projectIDs []string) error {
	for _, pid := range projectIDs {
		trimmed := strings.TrimSpace(pid)
		if trimmed == "" {
			continue
		}
		isChecked, errCheck := checkCloudAPIIsEnabled(ctx, httpClient, trimmed)
		if errCheck != nil {
			return fmt.Errorf("project %s: %w", trimmed, errCheck)
		}
		if !isChecked {
			return fmt.Errorf("project %s: Cloud AI API not enabled", trimmed)
		}
	}
	return nil
}

func fetchGCPProjects(ctx context.Context, httpClient *http.Client) ([]interfaces.GCPProjectProjects, error) {
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, "https://cloudresourcemanager.googleapis.com/v1/projects", nil)
	if errRequest != nil {
		return nil, fmt.Errorf("could not create project list request: %w", errRequest)
	}

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("failed to execute project list request: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("project list request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	var projects interfaces.GCPProject
	if errDecode := json.NewDecoder(resp.Body).Decode(&projects); errDecode != nil {
		return nil, fmt.Errorf("failed to unmarshal project list: %w", errDecode)
	}

	return projects.Projects, nil
}

func generateKiroPKCE() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, errRead := io.ReadFull(rand.Reader, b); errRead != nil {
		return "", "", fmt.Errorf("failed to generate random bytes: %w", errRead)
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)

	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])

	return verifier, challenge, nil
}

func gitLabAccountIdentifier(user *gitlabauth.User) string {
	if user == nil {
		return "user"
	}
	for _, value := range []string{user.Username, primaryGitLabEmail(user), user.Name} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return "user"
}

func gitLabBaseURLFromRequest(c *gin.Context) string {
	if c != nil {
		if raw := strings.TrimSpace(c.Query("base_url")); raw != "" {
			return gitlabauth.NormalizeBaseURL(raw)
		}
	}
	if raw := strings.TrimSpace(os.Getenv("GITLAB_BASE_URL")); raw != "" {
		return gitlabauth.NormalizeBaseURL(raw)
	}
	return gitlabauth.DefaultBaseURL
}

func maskGitLabToken(token string) string {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) <= 8 {
		return trimmed
	}
	return trimmed[:4] + "..." + trimmed[len(trimmed)-4:]
}

func mergeGitLabDirectAccessMetadata(metadata map[string]any, direct *gitlabauth.DirectAccessResponse) {
	if metadata == nil || direct == nil {
		return
	}
	if base := strings.TrimSpace(direct.BaseURL); base != "" {
		metadata["duo_gateway_base_url"] = base
	}
	if token := strings.TrimSpace(direct.Token); token != "" {
		metadata["duo_gateway_token"] = token
	}
	if direct.ExpiresAt > 0 {
		expiry := time.Unix(direct.ExpiresAt, 0).UTC()
		metadata["duo_gateway_expires_at"] = expiry.Format(time.RFC3339)
		now := time.Now().UTC()
		if ttl := expiry.Sub(now); ttl > 0 {
			interval := int(ttl.Seconds()) / 2
			switch {
			case interval < 60:
				interval = 60
			case interval > 240:
				interval = 240
			}
			metadata["refresh_interval_seconds"] = interval
		}
	}
	if len(direct.Headers) > 0 {
		headers := make(map[string]string, len(direct.Headers))
		for key, value := range direct.Headers {
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			if key == "" || value == "" {
				continue
			}
			headers[key] = value
		}
		if len(headers) > 0 {
			metadata["duo_gateway_headers"] = headers
		}
	}
	if direct.ModelDetails != nil {
		modelDetails := map[string]any{}
		if provider := strings.TrimSpace(direct.ModelDetails.ModelProvider); provider != "" {
			modelDetails["model_provider"] = provider
			metadata["model_provider"] = provider
		}
		if model := strings.TrimSpace(direct.ModelDetails.ModelName); model != "" {
			modelDetails["model_name"] = model
			metadata["model_name"] = model
		}
		if len(modelDetails) > 0 {
			metadata["model_details"] = modelDetails
		}
	}
}

func metadataFromAuthFileBytes(data []byte) map[string]any {
	if len(data) == 0 {
		return nil
	}
	metadata := make(map[string]any)
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil
	}
	return metadata
}

func normalizeKiroIDETokenMetadata(metadata map[string]any) (map[string]any, bool) {
	if len(metadata) == 0 {
		return nil, false
	}
	if existingType, _ := metadata["type"].(string); strings.TrimSpace(existingType) != "" {
		return nil, false
	}
	accessToken := strings.TrimSpace(stringFromMetadata(metadata, "accessToken"))
	if accessToken == "" {
		return nil, false
	}
	hasKiroIDEField := false
	for _, key := range []string{"profileArn", "clientIdHash", "startUrl"} {
		if strings.TrimSpace(stringFromMetadata(metadata, key)) != "" {
			hasKiroIDEField = true
			break
		}
	}
	if !hasKiroIDEField {
		return nil, false
	}

	email := strings.TrimSpace(stringFromMetadata(metadata, "email"))
	if email == "" {
		email = kiroauth.ExtractEmailFromJWT(accessToken)
	}
	return map[string]any{
		"type":           "kiro",
		"access_token":   accessToken,
		"refresh_token":  strings.TrimSpace(stringFromMetadata(metadata, "refreshToken")),
		"profile_arn":    strings.TrimSpace(stringFromMetadata(metadata, "profileArn")),
		"expires_at":     strings.TrimSpace(stringFromMetadata(metadata, "expiresAt")),
		"auth_method":    strings.ToLower(strings.TrimSpace(stringFromMetadata(metadata, "authMethod"))),
		"provider":       strings.TrimSpace(stringFromMetadata(metadata, "provider")),
		"client_id":      strings.TrimSpace(stringFromMetadata(metadata, "clientId")),
		"client_secret":  strings.TrimSpace(stringFromMetadata(metadata, "clientSecret")),
		"client_id_hash": strings.TrimSpace(stringFromMetadata(metadata, "clientIdHash")),
		"email":          email,
		"start_url":      strings.TrimSpace(stringFromMetadata(metadata, "startUrl")),
		"region":         strings.TrimSpace(stringFromMetadata(metadata, "region")),
	}, true
}

func (h *Handler) notifyAuthFilePersisted(ctx context.Context, auth *coreauth.Auth) error {
	if h == nil || h.postAuthPersistHook == nil || auth == nil {
		return nil
	}
	return h.postAuthPersistHook(ctx, auth)
}

func onboardAllGeminiProjects(ctx context.Context, httpClient *http.Client, storage *geminiAuth.GeminiTokenStorage) ([]string, error) {
	projects, errProjects := fetchGCPProjects(ctx, httpClient)
	if errProjects != nil {
		return nil, fmt.Errorf("fetch project list: %w", errProjects)
	}
	if len(projects) == 0 {
		return nil, fmt.Errorf("no Google Cloud projects available for this account")
	}
	activated := make([]string, 0, len(projects))
	seen := make(map[string]struct{}, len(projects))
	for _, project := range projects {
		candidate := strings.TrimSpace(project.ProjectID)
		if candidate == "" {
			continue
		}
		if _, dup := seen[candidate]; dup {
			continue
		}
		if err := performGeminiCLISetup(ctx, httpClient, storage, candidate); err != nil {
			return nil, fmt.Errorf("onboard project %s: %w", candidate, err)
		}
		finalID := strings.TrimSpace(storage.ProjectID)
		if finalID == "" {
			finalID = candidate
		}
		activated = append(activated, finalID)
		seen[candidate] = struct{}{}
	}
	if len(activated) == 0 {
		return nil, fmt.Errorf("no Google Cloud projects available for this account")
	}
	return activated, nil
}

func performGeminiCLISetup(ctx context.Context, httpClient *http.Client, storage *geminiAuth.GeminiTokenStorage, requestedProject string) error {
	metadata := map[string]string{
		"ideType":    "IDE_UNSPECIFIED",
		"platform":   "PLATFORM_UNSPECIFIED",
		"pluginType": "GEMINI",
	}

	trimmedRequest := strings.TrimSpace(requestedProject)
	explicitProject := trimmedRequest != ""

	loadReqBody := map[string]any{
		"metadata": metadata,
	}
	if explicitProject {
		loadReqBody["cloudaicompanionProject"] = trimmedRequest
	}

	var loadResp map[string]any
	if errLoad := callGeminiCLI(ctx, httpClient, "loadCodeAssist", loadReqBody, &loadResp); errLoad != nil {
		return fmt.Errorf("load code assist: %w", errLoad)
	}

	tierID := "legacy-tier"
	if tiers, okTiers := loadResp["allowedTiers"].([]any); okTiers {
		for _, rawTier := range tiers {
			tier, okTier := rawTier.(map[string]any)
			if !okTier {
				continue
			}
			if isDefault, okDefault := tier["isDefault"].(bool); okDefault && isDefault {
				if id, okID := tier["id"].(string); okID && strings.TrimSpace(id) != "" {
					tierID = strings.TrimSpace(id)
					break
				}
			}
		}
	}

	projectID := trimmedRequest
	if projectID == "" {
		if id, okProject := loadResp["cloudaicompanionProject"].(string); okProject {
			projectID = strings.TrimSpace(id)
		}
		if projectID == "" {
			if projectMap, okProject := loadResp["cloudaicompanionProject"].(map[string]any); okProject {
				if id, okID := projectMap["id"].(string); okID {
					projectID = strings.TrimSpace(id)
				}
			}
		}
	}
	if projectID == "" {
		// Auto-discovery: try onboardUser without specifying a project
		// to let Google auto-provision one (matches Gemini CLI headless behavior
		// and Antigravity's FetchProjectID pattern).
		autoOnboardReq := map[string]any{
			"tierId":   tierID,
			"metadata": metadata,
		}

		autoCtx, autoCancel := context.WithTimeout(ctx, 30*time.Second)
		defer autoCancel()
		for attempt := 1; ; attempt++ {
			var onboardResp map[string]any
			if errOnboard := callGeminiCLI(autoCtx, httpClient, "onboardUser", autoOnboardReq, &onboardResp); errOnboard != nil {
				return fmt.Errorf("auto-discovery onboardUser: %w", errOnboard)
			}

			if done, okDone := onboardResp["done"].(bool); okDone && done {
				if resp, okResp := onboardResp["response"].(map[string]any); okResp {
					switch v := resp["cloudaicompanionProject"].(type) {
					case string:
						projectID = strings.TrimSpace(v)
					case map[string]any:
						if id, okID := v["id"].(string); okID {
							projectID = strings.TrimSpace(id)
						}
					}
				}
				break
			}

			log.Debugf("Auto-discovery: onboarding in progress, attempt %d...", attempt)
			select {
			case <-autoCtx.Done():
				return &projectSelectionRequiredError{}
			case <-time.After(2 * time.Second):
			}
		}

		if projectID == "" {
			return &projectSelectionRequiredError{}
		}
		log.Infof("Auto-discovered project ID via onboarding: %s", projectID)
	}

	onboardReqBody := map[string]any{
		"tierId":                  tierID,
		"metadata":                metadata,
		"cloudaicompanionProject": projectID,
	}

	storage.ProjectID = projectID

	for {
		var onboardResp map[string]any
		if errOnboard := callGeminiCLI(ctx, httpClient, "onboardUser", onboardReqBody, &onboardResp); errOnboard != nil {
			return fmt.Errorf("onboard user: %w", errOnboard)
		}

		if done, okDone := onboardResp["done"].(bool); okDone && done {
			responseProjectID := ""
			if resp, okResp := onboardResp["response"].(map[string]any); okResp {
				switch projectValue := resp["cloudaicompanionProject"].(type) {
				case map[string]any:
					if id, okID := projectValue["id"].(string); okID {
						responseProjectID = strings.TrimSpace(id)
					}
				case string:
					responseProjectID = strings.TrimSpace(projectValue)
				}
			}

			finalProjectID := projectID
			if responseProjectID != "" {
				if explicitProject && !strings.EqualFold(responseProjectID, projectID) {
					log.Infof("Gemini onboarding: requested project %s maps to backend project %s", projectID, responseProjectID)
					log.Infof("Using backend project ID: %s", responseProjectID)
				}
				finalProjectID = responseProjectID
			}

			storage.ProjectID = strings.TrimSpace(finalProjectID)
			if storage.ProjectID == "" {
				storage.ProjectID = strings.TrimSpace(projectID)
			}
			if storage.ProjectID == "" {
				return fmt.Errorf("onboard user completed without project id")
			}
			log.Infof("Onboarding complete. Using Project ID: %s", storage.ProjectID)
			return nil
		}

		log.Println("Onboarding in progress, waiting 5 seconds...")
		time.Sleep(5 * time.Second)
	}
}

func primaryGitLabEmail(user *gitlabauth.User) string {
	if user == nil {
		return ""
	}
	if value := strings.TrimSpace(user.Email); value != "" {
		return value
	}
	return strings.TrimSpace(user.PublicEmail)
}

func sanitizeGitLabFileName(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return "user"
	}
	var builder strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
			lastDash = false
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_' || r == '.':
			builder.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				builder.WriteRune('-')
				lastDash = true
			}
		}
	}
	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return "user"
	}
	return result
}

func stringFromMetadata(metadata map[string]any, key string) string {
	raw, ok := metadata[key]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return value
	case fmt.Stringer:
		return value.String()
	default:
		return fmt.Sprint(value)
	}
}

func syncAuthFileExcludedModelsAttribute(auth *coreauth.Auth, touchedRoots map[string]struct{}) {
	if auth == nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	sourceKey := "excluded_models"
	if _, okUnderscore := touchedRoots["excluded_models"]; !okUnderscore {
		if _, okHyphen := touchedRoots["excluded-models"]; okHyphen {
			sourceKey = "excluded-models"
		}
	}
	excluded := authFileExcludedModelsValue(auth.Metadata[sourceKey])
	excluded = internalconfig.NormalizeExcludedModels(excluded)
	delete(auth.Metadata, "excluded-models")
	if len(excluded) == 0 {
		delete(auth.Metadata, "excluded_models")
		delete(auth.Attributes, "excluded_models")
		delete(auth.Attributes, "excluded_models_hash")
		return
	}
	auth.Metadata["excluded_models"] = append([]string(nil), excluded...)
	auth.Attributes["excluded_models"] = strings.Join(excluded, ",")
	auth.Attributes["excluded_models_hash"] = diff.ComputeExcludedModelsHash(excluded)
}
