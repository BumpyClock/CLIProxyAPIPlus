package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestPlusOAuthCommitReportsOutcomeForOnlyItsSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, failSave := range []bool{false, true} {
		name := "success"
		if failSave {
			name = "save failure"
		}
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/oauth/token":
					_, _ = w.Write([]byte(`{"access_token":"synthetic-test-only","token_type":"Bearer","expires_in":3600}`))
				case "/api/v4/user":
					_, _ = w.Write([]byte(`{"id":42,"username":"synthetic-user","email":"synthetic@example.invalid"}`))
				case "/api/v4/code_suggestions/direct_access":
					_, _ = w.Write([]byte(`{"base_url":"https://provider.example.invalid","token":"synthetic-gateway"}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			authDir := t.TempDir()
			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
			h.tokenStore = sdkauth.NewFileTokenStore()
			if failSave {
				h.postAuthHook = func(context.Context, *coreauth.Auth) error {
					return errors.New("synthetic persistence failure")
				}
			}
			store := newOAuthSessionStore(time.Minute)
			replaceOAuthSessionStoreForTest(t, store)
			store.Register("another-gitlab-login", "gitlab")
			router := gin.New()
			router.GET("/v0/management/gitlab-auth-url", h.RequestGitLabToken)
			router.GET("/v8/management/oauth/status", h.GetAuthStatus)
			start := httptest.NewRecorder()
			router.ServeHTTP(start, httptest.NewRequest(http.MethodGet, "/v0/management/gitlab-auth-url?base_url="+url.QueryEscape(upstream.URL)+"&client_id=synthetic-client", nil))
			if start.Code != http.StatusOK {
				t.Fatalf("start status=%d body=%s", start.Code, start.Body.String())
			}
			var login struct{ State string }
			if err := json.Unmarshal(start.Body.Bytes(), &login); err != nil || login.State == "" {
				t.Fatalf("invalid login response: %s, error=%v", start.Body.String(), err)
			}
			t.Cleanup(func() { store.Complete(login.State) })
			if _, err := WriteOAuthCallbackFileForPendingSession(authDir, "gitlab", login.State, "synthetic-code", ""); err != nil {
				t.Fatal(err)
			}
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				response := httptest.NewRecorder()
				router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/status?state="+login.State, nil))
				var status struct{ Status, Error string }
				if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
					t.Fatal(err)
				}
				if status.Status != "wait" {
					want := "ok"
					if failSave {
						want = "error"
						if !strings.Contains(status.Error, "Failed to save authentication tokens") {
							t.Fatalf("save error was not exposed: %#v", status)
						}
					}
					if status.Status != want {
						t.Fatalf("status=%#v, want %s", status, want)
					}
					if !store.IsPending("another-gitlab-login", "gitlab") {
						t.Fatal("one login completed another pending login")
					}
					return
				}
				select {
				case <-deadline.C:
					t.Fatal("OAuth remained pending after credential persistence finished")
				case <-tick.C:
				}
			}
		})
	}
}
