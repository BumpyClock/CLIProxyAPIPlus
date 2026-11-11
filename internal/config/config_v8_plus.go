package config

// Plus settings retain their native paths in both layouts. Migration, validation,
// and layout detection share this list so a v8 write cannot archive active settings.
var v8PlusNativeRoots = []string{
	"fallback-chain",
	"kiro",
	"kiro-fingerprint",
	"kiro-preferred-endpoint",
	"kiro-rate-limit",
	"kiro-system-prompt-inject-enable",
	"kiro-truncation-detector-enable",
	"kiro-extract-thinking-tag-enable",
	"ampcode",
	"incognito-browser",
}
