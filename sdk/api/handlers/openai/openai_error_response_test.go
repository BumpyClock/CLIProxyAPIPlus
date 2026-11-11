package openai

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

func TestOpenAIWriteErrorResponseSanitizesAtBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := &handlers.BaseAPIHandler{}
	for _, write := range []func(*gin.Context, *interfaces.ErrorMessage){
		NewOpenAIAPIHandler(base).WriteErrorResponse,
		NewOpenAIResponsesAPIHandler(base).WriteErrorResponse,
	} {
		for _, direct := range []bool{false, true} {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
			message := &interfaces.ErrorMessage{
				StatusCode:     http.StatusBadGateway,
				Error:          errors.New("upstream rejected api_key=synthetic-test-only"),
				DirectResponse: direct,
				Body:           []byte("synthetic-raw-body"),
			}
			write(c, message)
			body := recorder.Body.String()
			if recorder.Code != http.StatusBadGateway || !strings.Contains(body, "[REDACTED]") {
				t.Fatalf("status=%d body=%q", recorder.Code, body)
			}
			if strings.Contains(body, "synthetic-test-only") || strings.Contains(body, "synthetic-raw-body") {
				t.Fatalf("error response leaked executor data: %q", body)
			}
		}
	}
}

func TestSanitizeOpenAIErrorMessageIsIdempotent(t *testing.T) {
	input := &interfaces.ErrorMessage{
		StatusCode: http.StatusUnauthorized,
		Error:      errors.New(`{"error":{"message":"api_key=synthetic-test-only","code":"invalid_api_key"}}`),
	}
	once := sanitizeOpenAIErrorMessage(input)
	twice := sanitizeOpenAIErrorMessage(once)
	if twice.StatusCode != once.StatusCode || twice.Error.Error() != once.Error.Error() || twice.DirectResponse || twice.Body != nil {
		t.Fatalf("repeated boundary handling changed sanitized error: once=%#v twice=%#v", once, twice)
	}
	if strings.Contains(twice.Error.Error(), "synthetic-test-only") {
		t.Fatal("repeated sanitization restored executor data")
	}
}
