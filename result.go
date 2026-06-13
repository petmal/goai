package goai

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/zendev-sh/goai/provider"
)

// buildTextResult constructs a TextResult from accumulated steps.
// Caller must ensure steps is non-empty.
func buildTextResult(steps []StepResult, totalUsage provider.Usage) *TextResult {
	if len(steps) == 0 {
		return &TextResult{TotalUsage: totalUsage}
	}
	last := steps[len(steps)-1]
	// Accumulate text from all steps.
	var allText strings.Builder
	for _, s := range steps {
		allText.WriteString(s.Text)
	}
	// Accumulate reasoning text from all steps. Concatenated as-is so
	// callers see the same boundaries the steps had; consumers wanting
	// per-step reasoning can iterate Steps directly.
	var allReasoning strings.Builder
	for _, s := range steps {
		allReasoning.WriteString(s.Reasoning)
	}
	// Collect sources from all steps.
	var allSources []provider.Source
	for _, s := range steps {
		allSources = append(allSources, s.Sources...)
	}

	return &TextResult{
		Text:             allText.String(),
		Reasoning:        allReasoning.String(),
		ToolCalls:        last.ToolCalls,
		Steps:            steps,
		TotalUsage:       totalUsage,
		FinishReason:     last.FinishReason,
		Response:         last.Response,
		ProviderMetadata: last.ProviderMetadata,
		Sources:          allSources,
	}
}

// buildResponseMessages constructs the full ResponseMessages from the tool round-trip
// messages (delta between original and final params.Messages) and the steps.
//
// With Vercel-parity StopWhen placement (evaluated AFTER tool execution and
// appendToolRoundTrip), the delta always contains the assistant + tool-result
// messages for every completed step whose LLM response carried tool calls --
// including the last step on a StopWhen break and at MaxSteps exhaustion. The
// only case the delta is missing the last step's assistant message is a
// natural termination (last step produced text with no tool calls): that
// message is appended here.
//
// Consecutive tool messages are merged into a single message because some
// providers require parallel tool results in a single message (not split
// across multiple messages).
//
// The reasoning parameter provides thinking/reasoning parts for the final
// assistant message (streaming path only; GenerateText passes nil). It is
// only applied when the delta is empty or the last step had no tool calls
// (i.e. when we are actually building the final assistant message here).
func buildResponseMessages(roundTripDelta []provider.Message, steps []StepResult, reasoning []provider.Part) []provider.Message {
	if len(steps) == 0 {
		return nil
	}
	last := steps[len(steps)-1]
	if len(roundTripDelta) == 0 {
		return buildFinalAssistantMessages(last.Text, last.ToolCalls, reasoning)
	}
	msgs := mergeToolMessages(roundTripDelta)
	if len(last.ToolCalls) > 0 {
		// Delta already contains this step's assistant + tool-result messages
		// (appendToolRoundTrip ran before loop break). Avoid duplication.
		return msgs
	}
	// Natural termination: last step produced text with no tool calls - its
	// assistant message is NOT yet in the delta.
	finalMsg := buildFinalAssistantMessages(last.Text, last.ToolCalls, reasoning)
	return append(msgs, finalMsg...)
}

// mergeToolMessages merges consecutive tool-role messages into single messages.
// The internal tool loop creates one message per tool call, but callers expect
// parallel tool results grouped in a single message per round-trip.
func mergeToolMessages(msgs []provider.Message) []provider.Message {
	out := make([]provider.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == provider.RoleTool && len(out) > 0 && out[len(out)-1].Role == provider.RoleTool {
			// Merge parts into the previous tool message.
			out[len(out)-1].Content = append(out[len(out)-1].Content, m.Content...)
		} else {
			// Clone the message to avoid mutating the original.
			out = append(out, provider.Message{
				Role:    m.Role,
				Content: slices.Clone(m.Content),
			})
		}
	}
	return out
}

// buildFinalAssistantMessages builds a single assistant message from text, tool calls,
// and/or reasoning parts. Returns nil when all inputs are empty.
// Reasoning parts are placed first so providers that require thinking blocks
// (e.g. Bedrock with extended thinking) see them before text/tool_use content.
func buildFinalAssistantMessages(text string, toolCalls []provider.ToolCall, reasoning []provider.Part) []provider.Message {
	var parts []provider.Part
	parts = append(reasoning, parts...)
	if text != "" {
		parts = append(parts, provider.Part{Type: provider.PartText, Text: text})
	}
	for _, tc := range toolCalls {
		parts = append(parts, provider.Part{
			Type:       provider.PartToolCall,
			ToolCallID: tc.ID,
			ToolName:   tc.Name,
			ToolInput:  append(json.RawMessage(nil), tc.Input...),
			// Shallow copy of Metadata , matches the existing appendToolRoundTrip pattern.
			// Nested map values are shared, not deep-cloned.
			ProviderOptions: maps.Clone(tc.Metadata),
		})
	}
	if len(parts) == 0 {
		return nil
	}
	return []provider.Message{{Role: provider.RoleAssistant, Content: parts}}
}
