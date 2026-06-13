package goai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/zendev-sh/goai/provider"
)

// isProviderExecuted reports whether a tool call was executed by the provider
// (e.g. Anthropic web_search, OpenAI web_search_call). The provider returns
// the result inline on the assistant turn, so goai must not look it up in
// the user's tool map or synthesize a tool-result message for it.
//
// Providers mark these calls by setting tc.Metadata["providerExecuted"] = true
// (or by attaching a non-nil "resultBlock" / "rawItem" payload that the
// request serializer re-emits verbatim).
func isProviderExecuted(tc provider.ToolCall) bool {
	if tc.Metadata == nil {
		return false
	}
	if v, ok := tc.Metadata["providerExecuted"].(bool); ok && v {
		return true
	}
	if _, ok := tc.Metadata["resultBlock"]; ok {
		return true
	}
	if _, ok := tc.Metadata["rawItem"]; ok {
		return true
	}
	return false
}

// buildToolMap creates a name→Tool lookup of executable tools from the options.
//
// Tool names must be unique across all tools, not just executable ones: the
// Vercel AI SDK (our reference) keys its tool set by name, so a collision there
// is impossible. We validate every named tool to match that guarantee, while
// the returned map only contains tools that have an Execute function.
func buildToolMap(tools []Tool) (map[string]Tool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	m := make(map[string]Tool, len(tools))
	seen := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		if t.Name == "" {
			return nil, fmt.Errorf("goai: tool name must not be empty")
		}
		if _, exists := seen[t.Name]; exists {
			return nil, fmt.Errorf("goai: duplicate tool name %q: tool names must be unique", t.Name)
		}
		seen[t.Name] = struct{}{}
		if t.Execute != nil {
			m[t.Name] = t
		}
	}
	if len(m) == 0 {
		return nil, nil
	}
	return m, nil
}

// toolOutput holds the result of a single tool execution (package-level type
// shared between executeToolsParallel and buildToolMessages).
type toolOutput struct {
	index  int
	result string
	err    error
}

// toolHooks bundles the hook functions and options passed to executeToolsParallel.
type toolHooks struct {
	sequential      bool // when true, execute tools one at a time
	onToolCallStart []func(ToolCallStartInfo)
	onToolCall      []func(ToolCallInfo)
	onBeforeExecute func(BeforeToolExecuteInfo) BeforeToolExecuteResult
	onAfterExecute  func(AfterToolExecuteInfo) AfterToolExecuteResult
	onPanic         []func(PanicInfo)
}

func executeToolsParallel(
	ctx context.Context,
	calls []provider.ToolCall,
	toolMap map[string]Tool,
	step int,
	hooks toolHooks,
) ([]provider.Message, []provider.ToolResult) {

	results := make([]toolOutput, len(calls))
	var wg sync.WaitGroup

	for i, tc := range calls {
		// Server-executed tool calls (e.g. Anthropic web_search, OpenAI
		// web_search_call) are run by the provider itself; their results are
		// already inline on the assistant turn. They have no Execute and must
		// not be looked up in toolMap or surfaced as unknown-tool errors.
		if isProviderExecuted(tc) {
			results[i] = toolOutput{index: i, result: ""}
			continue
		}
		tool, ok := toolMap[tc.Name]
		if !ok {
			// Unknown tool: fire OnToolCallStart + OnToolCall with ErrUnknownTool.
			// Each hook is independently recover-wrapped so a panic in OnToolCallStart
			// does not prevent OnToolCall from firing.
			//
			// Asymmetry with known-tool path (below): for known tools, OnToolCallStart
			// panic prevents Execute from running (the tool should not execute if the
			// pre-hook crashed). For unknown tools, Execute never runs anyway, so both
			// hooks fire independently for observability completeness.
			results[i] = toolOutput{index: i, err: ErrUnknownTool}
			for _, fn := range hooks.onToolCallStart {
				func(f func(ToolCallStartInfo)) {
					defer func() {
						if r := recover(); r != nil {
							firePanic(hooks.onPanic, "OnToolCallStart", r)
						}
					}()
					f(ToolCallStartInfo{ToolCallID: tc.ID, ToolName: tc.Name, Step: step, Input: tc.Input})
				}(fn)
			}
			for _, fn := range hooks.onToolCall {
				func(f func(ToolCallInfo)) {
					defer func() {
						if r := recover(); r != nil {
							firePanic(hooks.onPanic, "OnToolCall", r)
						}
					}()
					f(ToolCallInfo{
						ToolCallID: tc.ID,
						ToolName:   tc.Name,
						Step:       step,
						Input:      tc.Input,
						Error:      ErrUnknownTool,
					})
				}(fn)
			}
			continue
		}

		toolFn := func(i int, tc provider.ToolCall, tool Tool) {
			if !hooks.sequential {
				defer wg.Done()
			}
			var hookFired bool // true after OnToolCallStart completes (before Execute)
			var executed bool  // tracks whether Execute ran (for panic recovery)
			defer func() {
				if r := recover(); r != nil {
					if !executed {
						// The full panic value + stack go to OnPanic (firePanic);
						// the tool error sent to the LLM omits the raw value so a
						// panic carrying sensitive data is not echoed to the model.
						if !hookFired {
							firePanic(hooks.onPanic, "OnToolCallStart", r)
							results[i] = toolOutput{index: i, err: fmt.Errorf("goai: OnToolCallStart hook for tool %q panicked", tc.Name)}
						} else {
							firePanic(hooks.onPanic, "tool:"+tc.Name, r)
							results[i] = toolOutput{index: i, err: fmt.Errorf("goai: tool %q panicked", tc.Name)}
						}
					}
					// executed==true: Execute succeeded, results[i] already set.
					// OnToolCall panic after Execute is swallowed (preserve result).
				}
			}()

			// OnToolCallStart: pre-execution (each independently recover-wrapped).
			var hookPanicked bool
			for _, fn := range hooks.onToolCallStart {
				func(f func(ToolCallStartInfo)) {
					defer func() {
						if r := recover(); r != nil {
							firePanic(hooks.onPanic, "OnToolCallStart", r)
							hookPanicked = true
						}
					}()
					f(ToolCallStartInfo{
						ToolCallID: tc.ID,
						ToolName:   tc.Name,
						Step:       step,
						Input:      tc.Input,
					})
				}(fn)
			}
			hookFired = true
			if hookPanicked {
				panicStr := fmt.Sprintf("goai: OnToolCallStart hook for tool %q panicked", tc.Name)
				results[i] = toolOutput{index: i, err: fmt.Errorf("%s", panicStr)}
				return
			}

			// Create tool context early so both hooks and Execute share it.
			toolCtx := context.WithValue(ctx, toolCallIDKey{}, tc.ID)

			// OnBeforeToolExecute: can skip execution (permission, doom loop, etc.).
			if hooks.onBeforeExecute != nil {
				var beforeResult BeforeToolExecuteResult
				func() {
					defer func() {
						if r := recover(); r != nil {
							firePanic(hooks.onPanic, "OnBeforeToolExecute", r)
							// Raw value goes to OnPanic only; omit it here so a
							// panic with sensitive data is not echoed to the model.
							beforeResult = BeforeToolExecuteResult{
								Skip:  true,
								Error: errors.New("goai: OnBeforeToolExecute hook panicked"),
							}
						}
					}()
					beforeResult = hooks.onBeforeExecute(BeforeToolExecuteInfo{
						Ctx:        toolCtx,
						ToolCallID: tc.ID,
						ToolName:   tc.Name,
						Step:       step,
						Input:      tc.Input,
					})
				}()
				if beforeResult.Skip {
					executed = true // prevent outer panic handler from overwriting
					if beforeResult.Error != nil {
						results[i] = toolOutput{index: i, result: beforeResult.Result, err: beforeResult.Error}
					} else {
						results[i] = toolOutput{index: i, result: beforeResult.Result}
					}
					// Fire OnToolCall with skipped result for observability.
					for _, fn := range hooks.onToolCall {
						func(f func(ToolCallInfo)) {
							defer func() {
								if r := recover(); r != nil {
									firePanic(hooks.onPanic, "OnToolCall", r)
								}
							}()
							f(ToolCallInfo{
								ToolCallID: tc.ID,
								ToolName:   tc.Name,
								Step:       step,
								Input:      tc.Input,
								Output:     beforeResult.Result,
								StartTime:  time.Now(),
								Skipped:    true,
								Error:      beforeResult.Error,
							})
						}(fn)
					}
					return
				}
				// Apply hook overrides for non-skipped tools.
				if beforeResult.Ctx != nil {
					toolCtx = beforeResult.Ctx
				}
				if beforeResult.Input != nil {
					tc.Input = beforeResult.Input
				}
			}

			start := time.Now()
			output, err := tool.Execute(toolCtx, tc.Input)
			executed = true

			// OnAfterToolExecute: can modify output (secret scanning, truncation, etc.).
			var afterMetadata map[string]any
			if hooks.onAfterExecute != nil {
				func() {
					defer func() {
						if r := recover(); r != nil {
							firePanic(hooks.onPanic, "OnAfterToolExecute", r)
							// Preserve original result on panic.
						}
					}()
					afterResult := hooks.onAfterExecute(AfterToolExecuteInfo{
						Ctx:        toolCtx,
						ToolCallID: tc.ID,
						ToolName:   tc.Name,
						Step:       step,
						Input:      tc.Input,
						Output:     output,
						Error:      err,
					})
					if afterResult.Output != "" {
						output = afterResult.Output
					}
					if afterResult.Error != nil {
						err = afterResult.Error
					}
					afterMetadata = afterResult.Metadata
				}()
			}

			results[i] = toolOutput{index: i, result: output, err: err}

			// OnToolCall: post-execution (each independently recover-wrapped).
			for _, fn := range hooks.onToolCall {
				func(f func(ToolCallInfo)) {
					defer func() {
						if r := recover(); r != nil {
							firePanic(hooks.onPanic, "OnToolCall", r)
						}
					}()
					info := ToolCallInfo{
						ToolCallID: tc.ID,
						ToolName:   tc.Name,
						Step:       step,
						Input:      tc.Input,
						Output:     output,
						StartTime:  start,
						Duration:   time.Since(start),
						Error:      err,
						Metadata:   afterMetadata,
					}
					var parsed any
					if err == nil && json.Unmarshal([]byte(output), &parsed) == nil {
						info.OutputObject = parsed
					}
					f(info)
				}(fn)
			}
		}
		if hooks.sequential {
			toolFn(i, tc, tool)
		} else {
			wg.Add(1)
			go toolFn(i, tc, tool)
		}
	}

	if !hooks.sequential {
		wg.Wait()
	}
	return buildToolMessages(calls, results), buildToolResults(calls, results)
}

// buildToolResults converts raw tool outputs into structured provider.ToolResult
// values (one per call, in call order). The Output string mirrors exactly what
// buildToolMessages places in the tool-result message content so consumers can
// correlate predicate input with the on-wire transcript.
func buildToolResults(calls []provider.ToolCall, results []toolOutput) []provider.ToolResult {
	if len(calls) == 0 {
		return nil
	}
	out := make([]provider.ToolResult, len(calls))
	for i, tc := range calls {
		r := results[i]
		tr := provider.ToolResult{
			ToolCallID: tc.ID,
			ToolName:   tc.Name,
			Error:      r.err,
			IsError:    r.err != nil,
		}
		if r.err != nil {
			errStr := r.err.Error()
			runes := []rune(errStr)
			if len(runes) > 500 {
				errStr = string(runes[:500]) + "..."
			}
			tr.Output = "error: " + errStr
		} else {
			tr.Output = r.result
		}
		out[i] = tr
	}
	return out
}

// buildToolMessages converts tool call results to provider messages.
// Note: toolOutput is defined as a package-level type (not function-scoped)
// so both executeToolsParallel and buildToolMessages can reference it.
func buildToolMessages(calls []provider.ToolCall, results []toolOutput) []provider.Message {
	msgs := make([]provider.Message, 0, len(calls))
	for i, tc := range calls {
		// Server-executed calls have no separate tool message; their result
		// is delivered inline on the assistant turn.
		if isProviderExecuted(tc) {
			continue
		}
		r := results[i]
		if r.err != nil {
			errStr := r.err.Error()
			runes := []rune(errStr)
			if len(runes) > 500 {
				errStr = string(runes[:500]) + "..."
			}
			msgs = append(msgs, ToolMessage(tc.ID, tc.Name, "error: "+errStr))
		} else {
			msgs = append(msgs, ToolMessage(tc.ID, tc.Name, r.result))
		}
	}
	return msgs
}

// appendToolRoundTrip appends an assistant message (with tool_use parts)
// and tool result messages for the streaming tool loop.
// reasoning parts are placed first so providers that require thinking blocks
// (e.g. Bedrock with extended thinking) see them before tool_use content.
func appendToolRoundTrip(
	msgs []provider.Message,
	text string,
	reasoning []provider.Part,
	toolCalls []provider.ToolCall,
	toolMsgs []provider.Message,
) []provider.Message {
	var parts []provider.Part
	// Reasoning first (before text and tool_use). Clone ProviderOptions to avoid
	// aliasing between params.Messages and ResponseMessages.
	for _, r := range reasoning {
		parts = append(parts, provider.Part{
			Type:            r.Type,
			Text:            r.Text,
			ProviderOptions: maps.Clone(r.ProviderOptions),
		})
	}
	if text != "" {
		parts = append(parts, provider.Part{Type: provider.PartText, Text: text})
	}
	for _, tc := range toolCalls {
		parts = append(parts, provider.Part{
			Type:            provider.PartToolCall,
			ToolCallID:      tc.ID,
			ToolName:        tc.Name,
			ToolInput:       append(json.RawMessage(nil), tc.Input...), // clone byte slice
			ProviderOptions: maps.Clone(tc.Metadata),                   // shallow clone (matches buildFinalAssistantMessages)
		})
	}
	msgs = append(msgs, provider.Message{Role: provider.RoleAssistant, Content: parts})
	msgs = slices.Concat(msgs, toolMsgs)
	return msgs
}
