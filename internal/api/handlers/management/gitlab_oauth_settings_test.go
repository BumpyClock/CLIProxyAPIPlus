package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestGitLabOAuthBindsServerSecretToConfiguredApplication(t *testing.T) {
	t.Setenv("GITLAB_BASE_URL", "https://gitlab.example.test")
	t.Setenv("GITLAB_OAUTH_CLIENT_ID", "fixture-id")
	t.Setenv("GITLAB_OAUTH_CLIENT_SECRET", "fixture-secret")
	for _, tc := range []struct {
		query, secret string
		wantError     bool
	}{
		{"", "fixture-secret", false},
		{"?base_url=https://gitlab.example.test/", "fixture-secret", false},
		{"?base_url=https://alternate.example.test&client_id=fixture-id", "", false},
		{"?client_id=other-id", "", false},
		{"?base_url=https://alternate.example.test", "", true},
		{"?base_url=http://alternate.example.test&client_id=fixture-id", "", true},
		{"?base_url=http://127.0.0.1:9999&client_id=fixture-id", "", false},
		{"?client_secret=fixture-query", "", true},
	} {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Set(ConfigV8ContextKey, true)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/start"+tc.query, nil)
		_, _, secret, err := gitLabOAuthClientSettings(ctx)
		if (err != nil) != tc.wantError || secret != tc.secret {
			t.Fatalf("%s: unexpected secret binding or validation result (%v)", tc.query, err)
		}
	}
}

func TestGitLabV8ImportRejectsInsecureConfiguredURL(t *testing.T) {
	t.Setenv("GITLAB_BASE_URL", "http://example.test")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(ConfigV8ContextKey, true)
	c.Request = httptest.NewRequest(http.MethodPost, "/oauth/import", strings.NewReader(`{"personal_access_token":"fixture-token"}`))
	(&Handler{}).RequestGitLabPATToken(c)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "must use HTTPS") {
		t.Fatalf("GitLab import HTTP %d: %s", w.Code, w.Body.String())
	}
}
