package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"gopkg.in/yaml.v3"
)

func TestConfigV8UnrelatedWritesRetainPlusSettings(t *testing.T) {
	gin.SetMode(gin.TestMode)
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "testdata", "plus-v8-native.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.GET("/v8/management/config", h.ConfigV8)
	router.PUT("/v8/management/config/*path", h.ConfigV8)
	router.PUT("/v0/management/debug", h.PutDebug)
	request := func(method, url, body, etag string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, url, strings.NewReader(body))
		if etag != "" {
			req.Header.Set("If-Match", etag)
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s %s: status=%d body=%s", method, url, recorder.Code, recorder.Body.String())
		}
		return recorder
	}
	var expected map[string]any
	if err = yaml.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	delete(expected, "port")
	assertSettings := func(actual, expected map[string]any) {
		t.Helper()
		for key, value := range expected {
			if !reflect.DeepEqual(actual[key], value) {
				t.Errorf("Plus setting %s lost: got %#v, want %#v", key, actual[key], value)
			}
		}
	}
	read := request(http.MethodGet, "/v8/management/config", "", "")
	var api map[string]any
	if err = json.Unmarshal(read.Body.Bytes(), &api); err != nil {
		t.Fatal(err)
	}
	// JSON numbers decode as float64; compare via the same boundary for both sides.
	expectedJSON, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	var expectedAPI map[string]any
	if err = json.Unmarshal(expectedJSON, &expectedAPI); err != nil {
		t.Fatal(err)
	}
	assertSettings(api, expectedAPI)
	etag := read.Header().Get("ETag")
	if etag == "" {
		t.Fatal("config read omitted ETag")
	}
	for _, step := range []struct{ url, body string }{
		{"/v8/management/config/observability/logs/debug", "true"},
		{"/v0/management/debug", `{"value":false}`},
	} {
		request(http.MethodPut, step.url, step.body, etag)
		saved, errRead := os.ReadFile(path)
		if errRead != nil {
			t.Fatal(errRead)
		}
		var active map[string]any
		if err = yaml.Unmarshal(saved, &active); err != nil {
			t.Fatal(err)
		}
		assertSettings(active, expected)
		if err = config.ValidateV8Config(saved); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(h.cfg.FallbackChain, cfg.FallbackChain) || !reflect.DeepEqual(h.cfg.KiroKey, cfg.KiroKey) || !reflect.DeepEqual(h.cfg.AmpCode, cfg.AmpCode) || !h.cfg.IncognitoBrowser {
			t.Fatal("unrelated config write lost runtime Plus settings")
		}
	}
}
