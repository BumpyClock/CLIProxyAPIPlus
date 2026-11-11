# v8 upstream integration review

## Review scope

Independent reviews used Claude Opus 5.5 and GPT 6.1 Sol with high reasoning. Both
initial reviews blocked approval. They examined first-parent reconciliation,
custom patches, and selected integration boundaries. Neither reviewed every
line of the upstream import. This record describes the repairs and remaining
limits, not a second model approval of the final revision.

## Findings repaired

- **Active Plus settings disappeared on v8 writes.** A shared native-root registry
  now feeds migration, validation, and layout detection. Regression fixtures cover
  all ten retained roots, v8 JSON reads, unrelated v8 writes, and legacy saves of
  v8 documents. The regression failed before the repair.
- **Failed Plus credential saves left OAuth stuck committing.** All eight
  Plus login save sites now use the core commit-and-complete operation. The
  duplicate save-state API is removed. A loopback GitLab lifecycle regression
  proved the pending-state failure before the repair.
- **One login completed another session for the same provider.** Provider-wide
  completion calls were removed from Plus login handlers. The same lifecycle
  regression reproduced this defect and now verifies independent session state.
- **OpenAI HTTP error redaction depended on individual call sites.** Both OpenAI
  handler types now sanitize at their HTTP error boundary. Thirty-four repeated
  wrappers and the duplicate pending-stream reader were removed. Separate SSE
  sinks retain their necessary sanitization. Boundary tests cover untrusted
  direct bodies, repeated sanitization, and trusted plugin response preservation.
  The intentionally trusted native Responses fixture now sets its trust marker.
- **OAuth fixtures used a nonexistent v8 polling path.** They now use
  `/v8/management/oauth/status`.
- **Copilot discovery checked policy state applied only later.** The unreachable
  guard and its synthetic-only test case were removed. Exact-ID matching and
  existing reasoning restrictions remain covered.
- **Six Go plugin examples retained v7 dependency versions under v8 paths.**
  Copilot's PR review exposed invalid manifests that the root Go suite cannot
  reach. Their requirements now match v8. A discovery-based gate tests every Go
  plugin module and builds its C-shared library with read-only module metadata.
  Both PR CI and the upstream-sync verification script run this gate.

The local repairs introduce no new file-size crossing above 1,000 lines.
Upstream size crossings remain imported implementation, not locally expanded
reconciliation. Decomposing unrelated core modules here would enlarge the sync
surface and make future upstream reconciliation harder to verify.

## Remaining limits

- Plus built-in OAuth, usage snapshots/export/import, and Copilot quota remain
  v0-only. Core and plugin operations use the native v8 contract. The management
  reference describes this boundary; native Plus v8 parity is not implemented.
- Core duplicates plugin login-entry normalization between management and the
  plugin host. Consolidation remains a follow-up with lifecycle coverage.
- Imported fallback-chain validation still runs on requests. Moving it to a
  single configuration boundary must cover file loading and programmatic config
  updates. This sync does not change its malformed-config behavior.
- Kiro presentation metadata still travels through its legacy status channel.
  Legacy provider handlers also retain their existing console output.
- No live provider sign-in, deployment, billable inference, or exhaustive review
  of imported translators and executors was performed.

## Verification

The final code passed `.github/scripts/verify-upstream-sync.sh` on macOS and a
no-secrets Boat Linux sandbox using Go 1.27.3. This includes a server build/help
check, the nested Go plugin example gate, the full ordinary Go suite, and
selected race tests. Linux effective
capabilities were cleared so unreadable-file tests exercised Unix permissions.

Additional Linux race checks passed for config, management session commits,
multi-entry plugin OAuth, pluginhost, and OpenAI error boundaries. The current
frontend's native v8 smoke passed all eight groups against the built fork using
a disposable loopback fixture. The server was stopped afterward.

An earlier integration run recorded a macOS WebRTC DataChannel failure. The final
full macOS run passed; no root-cause fix or proof of non-flakiness is claimed.
