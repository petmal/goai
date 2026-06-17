# Code Review: Go Idiomatics — Aggregated Findings

**Task**: dev-admin-QuickStart-202606132143
**Date**: 2026-06-17
**Reviewers**: Static Analysis (Agent 1) + Semantic Review (Agent 2), consolidated by Coordinator
**Overall Assessment**: 🔴 **Request Changes** — 2 critical, 5 high, 6 medium issues

---

## Conflicts Resolved

| Finding | Agent 1 | Agent 2 | Resolution |
|---------|---------|---------|------------|
| `GenerateText` variable shadowing (`err`) | Critical | Not flagged | **Downgraded to Info** — `err` at `generate.go:812` shadows named return, but `recoverToError` at line 724 writes to the named return. In panic path, function exits via defer chain using named returns. Normal path uses explicit `return nil, err` (local). Shadowing is harmless but confusing. |
| `context` unused import in openaicompat | Warning | Not flagged | **REJECTED** — `context.Context` is used by `ParseStream(ctx context.Context, ...)` at line 358. Agent 1 was wrong. |
| `StdioTransport.Start` goroutine leak | Not flagged | Critical | **Downgraded to Info** — `exec.CommandContext(ctx, ...)` at line 121 auto-kills subprocess on ctx cancellation, closing stdout/stderr pipes and unblocking goroutines. Agent 2 was wrong. |
| `CachedTokenSource` race | Major #6 | High #3 | **Agreed: High** — Both agents identified the same race. The guard at `token.go:89` only checks expiry, not freshness. |
| `ParseStream` JSON drop | Not flagged | Medium #10 | **Agreed: High** — Silent `continue` on malformed JSON at `openaicompat.go:389-391` is worse than medium. |
| MCP SSE goroutine leak | Major #4 | Critical #2 | **Agreed: Critical** — `context.Background()` at `transport.go:497` means goroutine outlives caller context. |

---

## 1. Critical Issues (Must Fix Before Merge)

### C1: `go.mod` declares non-existent Go 1.25.0
- **File**: `go.mod:3`
- **Source**: Agent 1 Critical #2 (verified by coordinator)
- **Impact**: `go build` fails on any real toolchain. Go 1.25 has not been released.
- **Fix**: Change to `go 1.24.0` or actual minimum supported version.

### C2: `HTTPTransport.Send` goroutine leak on SSE responses
- **File**: `mcp/transport.go:497`
- **Source**: Agent 2 Critical #2 + Agent 1 Major #4 (convergent)
- **Impact**: Every SSE POST spawns a goroutine with `context.Background()`. If `Close()` is never called, goroutine leaks. Even with `Close()`, the goroutine has no parent context timeout.
- **Fix**: Derive from transport lifecycle context, not `context.Background()`.

---

## 2. High Issues (Should Fix)

### H1: `SchemaFrom[*T]()` loses nullable distinction
- **File**: `schema.go:56-60`
- **Source**: Agent 1 Major #3 (verified by coordinator)
- **Impact**: `SchemaFrom[*MyStruct]()` and `SchemaFrom[MyStruct]()` produce identical schemas (both `{"type":"object"}`). Pointer unwrapping at line 57-60 defeats `typeToSchema`'s nullability logic.
- **Fix**: Remove pointer-unwrapping loop; let `typeToSchema` handle pointers. Or document that `SchemaFrom[*T]` ≡ `SchemaFrom[T]`.

### H2: `CachedTokenSource.Token` can overwrite fresher token
- **File**: `provider/token.go:86-92`
- **Source**: Agent 1 Major #6 + Agent 2 High #3 (convergent)
- **Impact**: Two goroutines fetch tokens concurrently. Slower goroutine may overwrite a fresher token because the guard only checks expiry, not freshness.
- **Fix**: Compare `token.ExpiresAt.After(c.cached.ExpiresAt)` instead of checking `c.cached` expiry.

### H3: `mergeEnv` inherits full parent environment (credential leak)
- **File**: `mcp/transport.go:242-251`
- **Source**: Agent 2 High #5 (verified by coordinator)
- **Impact**: `os.Environ()` passes ALL env vars (API keys, tokens, passwords) to child MCP server process. Code acknowledges this in comments but takes no action.
- **Fix**: Start from empty env by default. Add `WithStdioInheritEnv` option for opt-in inheritance.

### H4: MCP `handleMessage` blocking send can deadlock pipeline
- **File**: `mcp/client.go:411-412`
- **Source**: Not flagged by either agent (coordinator finding)
- **Impact**: `ch <- msg` blocks if receiver timed out. Stalls entire message processing pipeline.
- **Fix**: `select { case ch <- msg: default: }`

### H5: `ParseStream` silently drops malformed JSON
- **File**: `internal/openaicompat/openaicompat.go:389-391`
- **Source**: Agent 2 Medium #10 (escalated by coordinator)
- **Impact**: Text, tool calls, and finish reasons lost with no indication to caller. Masks server-side issues.
- **Fix**: Emit `ChunkError` for first malformed chunk, then continue.

---

## 3. Medium Issues (Nice to Have)

### M1: Shallow copy of nested `ProviderOptions`
- **File**: `generate.go:186`
- **Source**: Agent 1 Major #5
- **Impact**: `maps.Clone` is shallow. Nested maps/slices in `ProviderOptions` are shared with caller. Mutating after `buildParams` could corrupt request body.
- **Fix**: Deep-clone known nested types, or document the constraint.

### M2: `readSSEBodyCancellable` deadlock if `body.Close()` blocks
- **File**: `mcp/transport.go:511-524`
- **Source**: Agent 1 Major #4 + Agent 2 Medium #9 (convergent)
- **Impact**: On ctx cancellation, `body.Close()` + `<-done` can deadlock if close blocks.
- **Fix**: Add timeout to `<-done` wait.

### M3: `applyCaching` overwrites existing `CacheControl` on non-text parts
- **File**: `caching.go:26`
- **Source**: Agent 2 Medium #8 (verified by coordinator)
- **Impact**: Sets `CacheControl = "ephemeral"` on last content part regardless of type. Overwrites existing CacheControl. May cause API error on image parts.
- **Fix**: Only set on last TEXT part, only if `CacheControl` is empty.

### M4: `IsReasoningModel` heuristic is fragile
- **File**: `internal/openaicompat/openaicompat.go:230-242`
- **Source**: Agent 2 Medium #11
- **Impact**: `'o'` + digit prefix check has false positives for future models. `gpt-5` check excludes `gpt-5-chat` but misses other variants.
- **Fix**: Use explicit allowlist instead of heuristics.

### M5: `WithMaxRetries(-1)` sets 2 billion retries
- **File**: `options.go:252`
- **Source**: Agent 2 Medium #12
- **Impact**: `1<<31 - 1` retries with 60s cap = effectively infinite without context deadline.
- **Fix**: Cap at reasonable value (e.g., 1000) or require context deadline.

### M6: `onceCloser.Close()` panics on nil `closeFn`
- **File**: `provider/openai/openai.go:242-245`
- **Source**: Agent 1 Warning #8
- **Impact**: Unconditional `o.closeFn()` panics if field is nil. Currently only constructed with valid function, but type is fragile.
- **Fix**: Add nil guard: `if o.closeFn != nil { o.closeFn() }`.

---

## 4. Low Issues (Cosmetic)

| # | Issue | File | Source |
|---|-------|------|--------|
| L1 | `repairJSON` byte-by-byte iteration slow for large inputs | `partial_json.go:31-97` | Agent 2 Low #13 |
| L2 | `ParseStream` allocates empty `providerMeta` map per stream | `openaicompat.go:370` | Agent 2 Low #14 |
| L3 | `SSETransport.Close` doesn't wait for `readSSE` goroutine | `mcp/transport.go:783-792` | Agent 2 Low #15 |
| L4 | `extractTextContent` drops `type: "thinking"` in array content | `openaicompat.go:766-793` | Agent 2 Low #16 |
| L5 | `normalizeID` precision loss for float64 > int64 max | `mcp/client.go:501` | Agent 1 Warning #9 |
| L6 | `buildTextResult` returns zero-value for empty steps | `result.go:14-16` | Agent 1 Warning #10 |
| L7 | `go.sum` not committed | Workspace root | Agent 1 Warning #11 |
| L8 | Inconsistent error wrapping in `mcp/transport.go` | Multiple | Agent 1 Info #13 |

---

## 5. Positive Highlights (Both Agents Agreed)

1. **Excellent panic recovery** — Multi-layer: `callHook`, `recoverToError`, `recoverToStreamErr`, `firePanic`
2. **Lock-free token source** — `RWMutex` with fetch outside lock (per AGENTS.md rule)
3. **No input mutation** — Slices/maps cloned before modification throughout
4. **`errors.As` discipline** — No type assertions for errors anywhere
5. **Atomic `AgentState`** — Packed `uint64` for tear-free concurrent reads
6. **Comprehensive streaming architecture** — `consumeOnce` prevents double-consumption, `TrySend` prevents goroutine leaks
7. **Minimal dependencies** — 2 direct: `goleak`, `oauth2`
8. **Cycle-safe JSON schema** — `seen` map prevents infinite recursion
9. **SSE line-size bounding** — `MaxLineSize` constant prevents unbounded memory growth
10. **Well-documented fix tracking** — FIX 12, 21, 33, 34, 35, 47 provide clear audit trail

---

## 6. Previously Fixed Issues (Verified ✅)

All issues from prior reviews (`review-go-idiomatics-20260613.md`, `review-code-fixes-20260613.md`, `review-code-fixes-postfix-20260613.md`) remain correctly fixed:

| Issue | Fix | Status |
|-------|-----|--------|
| `wg.Go()` compile error | `wg.Add(1)` + `go func(idx, ch)` | ✅ Verified |
| `os` receiver shadowing | Renamed to `s`, removed `osStderr` | ✅ Verified |
| `generate.go` file split | 5 files, no circular deps | ✅ Verified |
| `highestInflightStep` redundant init | `:= 1` in both paths | ✅ Verified |
| Bare type assertion in `schema.go` | `, ok` idiom with fall-through | ✅ Verified |
| Empty tool name → error | Returns `fmt.Errorf("goai: ...")` | ✅ Verified |
| `retryable` documentation | Best-effort fallback documented | ✅ Verified |

---

## 7. Risk Assessment

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| Go 1.25.0 build failure | Certain | High | Trivial version string change |
| MCP SSE goroutine leak | Medium | High | Derive from lifecycle context |
| `SchemaFrom` nullable loss | High | Medium | Remove pointer unwrap loop |
| `CachedTokenSource` race | Low | Medium | Compare ExpiresAt, not expiry |
| `mergeEnv` credential leak | Medium | High | Empty-env default + opt-in |
| MCP blocking send deadlock | Low | High | Non-blocking select |
| Silent JSON drops | Medium | Medium | Emit ChunkError |
