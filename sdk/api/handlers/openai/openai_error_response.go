package openai

import (
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
)

// WriteErrorResponse sanitizes executor errors at the shared OpenAI HTTP boundary.
func (h *OpenAIAPIHandler) WriteErrorResponse(c *gin.Context, msg *interfaces.ErrorMessage) {
	h.BaseAPIHandler.WriteErrorResponse(c, sanitizeOpenAIErrorMessage(msg))
}

func (h *OpenAIResponsesAPIHandler) WriteErrorResponse(c *gin.Context, msg *interfaces.ErrorMessage) {
	h.BaseAPIHandler.WriteErrorResponse(c, sanitizeOpenAIErrorMessage(msg))
}
