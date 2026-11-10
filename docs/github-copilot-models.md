# GitHub Copilot model metadata

Copilot model display names start with `GH `. Routing IDs are unchanged, including
credential prefixes such as `copilot/gpt-6.1-sol`.

When Copilot's built-in definitions lack reasoning metadata, discovery fills it
from an exact model ID in the current canonical Codex client catalog, then the
static model catalog. Existing Copilot reasoning restrictions take precedence.
Static fallback requires the returned catalog ID to match exactly, even when the
v8 registry can resolve a dated or bracketed Claude variant to a family entry. If the Codex catalog
cannot be decoded, enrichment still labels models but leaves reasoning unchanged.
Unknown models do not gain inferred reasoning levels. Account-specific token
limits remain unchanged. This applies to both API discovery and static fallback.

Pi consumes the proxy's reasoning metadata through `/v1/models?client_version=pi`.
After changing proxy model routes, run `/cliproxyapi-refresh` in Pi to replace its
catalog cache. Automatic background refresh can retain disappeared models
temporarily, so it can show both old bare IDs and new prefixed IDs.

Release context: fixes missing thinking controls for newly discovered Copilot
models whose exact canonical counterparts advertise reasoning levels, and labels
Copilot routes without changing their public model IDs.
