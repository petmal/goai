package goai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/zendev-sh/goai/provider"
)

// TextStream is a streaming text generation response.
//
// Callers must consume the stream (via Stream, TextStream, or Result) or cancel
// the context. Discarding a TextStream without consuming leaks goroutines.
//
// It provides three consumption modes (Stream, TextStream, Result).
// Stream() and TextStream() are mutually exclusive - only call one.
// Result() can always be called, including after Stream() or TextStream(),
// to get the accumulated final result.
type TextStream struct {
	ctx           context.Context
	source        <-chan provider.StreamChunk
	consumeOnce   sync.Once
	doneCh        chan struct{}
	timeoutCancel context.CancelFunc

	// Channels returned by the first Stream()/TextStream() call.
	// Subsequent calls return the same channel instead of a dead one.
	rawCh  <-chan provider.StreamChunk
	textCh <-chan string

	// Hook support.
	onResponse   []func(ResponseInfo)
	onStepFinish []func(StepResult)
	onFinish     []func(FinishInfo)
	onPanic      []func(PanicInfo)
	startTime    time.Time

	// stateRef, when non-nil, is transitioned to StepIdle when the consume
	// goroutine returns. Only set by the single-shot StreamText path
	// (streamWithToolLoop owns its own StateRef lifecycle inline). A nil
	// stateRef is a no-op; AgentState.set handles nil receiver safely.
	// See FIX 34.
	stateRef *AgentState

	// Accumulated state (written by consume goroutine, read after doneCh closes).
	text             strings.Builder
	toolCalls        []provider.ToolCall
	sources          []provider.Source
	finishReason     provider.FinishReason
	usage            provider.Usage
	response         provider.ResponseMetadata
	providerMetadata map[string]map[string]any
	streamErr        error

	// Multi-step accumulation (written by consume goroutine).
	steps         []StepResult
	currentStep   int
	stepText      strings.Builder
	stepToolCalls []provider.ToolCall
	stepSources   []provider.Source
	reasoningBuf  strings.Builder // consolidated reasoning text (matches drainStep)
	reasoningMeta map[string]any  // merged reasoning metadata (e.g. Anthropic signature)

	// responseMessages is set by the streamWithToolLoop goroutine before doneCh closes.
	responseMessages []provider.Message
	// stepsExhausted is set by the streamWithToolLoop goroutine when MaxSteps was reached.
	stepsExhausted bool
}

func newTextStream(ctx context.Context, source <-chan provider.StreamChunk) *TextStream {
	return &TextStream{
		ctx:    ctx,
		source: source,
		doneCh: make(chan struct{}),
	}
}

// Stream returns a channel that emits raw StreamChunks from the provider.
// Mutually exclusive with TextStream() - only call one streaming method.
func (ts *TextStream) Stream() <-chan provider.StreamChunk {
	ch := make(chan provider.StreamChunk, 64)
	ts.consumeOnce.Do(func() {
		ts.rawCh = ch
		go ts.consume(ch, nil)
	})
	if ts.rawCh != nil {
		return ts.rawCh
	}
	// Called after TextStream() consumed the source - return closed channel.
	close(ch)
	return ch
}

// TextStream returns the underlying channel of text chunks.
// Note: this method has the same name as the containing type (TextStream);
// call it as stream.TextStream() to receive the channel.
// Mutually exclusive with Stream() - only call one streaming method.
func (ts *TextStream) TextStream() <-chan string {
	ch := make(chan string, 64)
	ts.consumeOnce.Do(func() {
		ts.textCh = ch
		go ts.consume(nil, ch)
	})
	if ts.textCh != nil {
		return ts.textCh
	}
	// Called after Stream() consumed the source - return closed channel.
	close(ch)
	return ch
}

// Result blocks until the stream completes and returns the accumulated result.
// Check Err() after Result() to detect stream errors - Result does not surface
// errors directly (use Err or check result.Steps for partial data).
// Can be called after Stream() or TextStream() to get accumulated data.
// Note: unlike ObjectStream.Result(), this method does not return an error.
// Call Err() after Result() to check for stream errors.
func (ts *TextStream) Result() *TextResult {
	ts.consumeOnce.Do(func() {
		go ts.consume(nil, nil)
	})
	<-ts.doneCh
	return ts.buildResult()
}

// Err returns the first stream error encountered, or nil.
// Must be called after the stream is fully consumed (after Result(),
// or after the Stream()/TextStream() channel is drained).
// Follows the bufio.Scanner.Err() pattern.
func (ts *TextStream) Err() error {
	<-ts.doneCh
	return ts.streamErr
}

func (ts *TextStream) consume(rawOut chan<- provider.StreamChunk, textOut chan<- string) {
	defer close(ts.doneCh)
	// Surface a panic from any user hook fired during teardown (OnStepFinish,
	// OnResponse, OnFinish) through stream.Err(). Registered near the top so it
	// runs after (LIFO) the hook defers below, catching panics they re-raise via
	// callHook. A pre-existing stream error is preserved as the root cause.
	defer recoverToStreamErr(ts.onPanic, "stream", func(e error) {
		if ts.streamErr == nil {
			ts.streamErr = e
		}
	})
	// FIX 34: transition StateRef to StepIdle as the consume goroutine
	// exits. Single-shot StreamText wires this (stateRef is nil for the
	// multi-step streamWithToolLoop path, which owns its own StateRef
	// transitions inline). Deferred second from the top so it runs just
	// before close(doneCh) - after all user hooks (OnResponse, OnStepFinish,
	// OnFinish) have fired, so a poller that observes StepIdle can assume
	// all hooks have already returned. Step count is 1 for a single-shot
	// stream (the one DoStream call), regardless of whether the provider
	// emitted any chunks.
	if ts.stateRef != nil {
		defer func() { ts.stateRef.set(StepIdle, 1) }()
	}
	if ts.timeoutCancel != nil {
		defer ts.timeoutCancel()
	}
	if rawOut != nil {
		defer close(rawOut)
	}
	if textOut != nil {
		defer close(textOut)
	}

	// Call OnFinish hook when consume finishes (single-step streaming only).
	// For multi-step streaming (streamWithToolLoop), OnFinish fires inline in the
	// goroutine and ts.onFinish is nil, so this block is skipped.
	// Deferred BEFORE OnStepFinish so it runs AFTER it (LIFO order).
	if len(ts.onFinish) > 0 {
		defer func() {
			fireOnFinish(ts.onPanic, ts.onFinish, FinishInfo{
				TotalSteps:   1,
				TotalUsage:   ts.usage,
				FinishReason: ts.finishReason,
				StoppedBy:    provider.StopCauseNatural,
			})
		}()
	}

	// Call OnStepFinish hook when consume finishes (single-step streaming only).
	// For multi-step streaming (streamWithToolLoop), OnStepFinish fires inline per
	// step and ts.onStepFinish is nil, so this block is skipped.
	// Deferred BEFORE OnResponse so it runs AFTER it (LIFO order), matching
	// GenerateText's OnResponse → OnStepFinish sequence.
	if len(ts.onStepFinish) > 0 {
		defer func() {
			stepResult := StepResult{
				Number:           1,
				Text:             ts.stepText.String(),
				ToolCalls:        ts.toolCalls,
				FinishReason:     ts.finishReason,
				Usage:            ts.usage,
				Response:         ts.response,
				Sources:          ts.sources,
				ProviderMetadata: ts.providerMetadata,
			}
			for _, fn := range ts.onStepFinish {
				callHook(ts.onPanic, "OnStepFinish", func() { fn(stepResult) })
			}
		}()
	}

	// Call OnResponse hook when consume finishes (after all chunks processed).
	if len(ts.onResponse) > 0 {
		defer func() {
			info := ResponseInfo{
				Latency:      time.Since(ts.startTime),
				Usage:        ts.usage,
				FinishReason: ts.finishReason,
				Error:        ts.streamErr,
			}
			var apiErr *APIError
			if errors.As(ts.streamErr, &apiErr) {
				info.StatusCode = apiErr.StatusCode
			}
			for _, fn := range ts.onResponse {
				callHook(ts.onPanic, "OnResponse", func() { fn(info) })
			}
		}()
	}

	for chunk := range ts.source {
		switch chunk.Type {
		case provider.ChunkText:
			ts.text.WriteString(chunk.Text)     // global accumulator (existing, includes reasoning)
			ts.stepText.WriteString(chunk.Text) // per-step text-only accumulator (new)
			if s, ok := chunk.Metadata["source"].(provider.Source); ok {
				ts.sources = append(ts.sources, s)         // global (existing)
				ts.stepSources = append(ts.stepSources, s) // per-step (new)
			}

		case provider.ChunkReasoning:
			ts.text.WriteString(chunk.Text) // global accumulator (existing, includes reasoning)
			// Consolidate reasoning fragments into one Part (matching drainStep behavior).
			// Text is accumulated; metadata is merged (last chunk carries the signature).
			if chunk.Text != "" {
				ts.reasoningBuf.WriteString(chunk.Text)
			}
			if chunk.Metadata != nil {
				if ts.reasoningMeta == nil {
					ts.reasoningMeta = make(map[string]any)
				}
				for k, v := range chunk.Metadata {
					ts.reasoningMeta[k] = v
				}
			}
			if s, ok := chunk.Metadata["source"].(provider.Source); ok {
				ts.sources = append(ts.sources, s)         // global (preserve existing behavior)
				ts.stepSources = append(ts.stepSources, s) // per-step
			}

		case provider.ChunkToolCall:
			tc := provider.ToolCall{ID: chunk.ToolCallID, Name: chunk.ToolName, Input: json.RawMessage(chunk.ToolInput), Metadata: chunk.Metadata}
			ts.toolCalls = append(ts.toolCalls, tc)
			ts.stepToolCalls = append(ts.stepToolCalls, tc)

		case provider.ChunkStepFinish:
			// GoAI-emitted tool-results backfill: update the last completed
			// step's ToolResults (FIX 7 streaming parity). Emitted after
			// executeToolsParallel returns so ts.steps exposes the same data
			// the sync path's StepResult.ToolResults carries.
			if stepSource, _ := chunk.Metadata["stepSource"].(string); stepSource == "goai-tool-results" {
				if trs, ok := chunk.Metadata["toolResults"].([]provider.ToolResult); ok && len(ts.steps) > 0 {
					ts.steps[len(ts.steps)-1].ToolResults = trs
				}
				continue
			}
			// GoAI-emitted step boundaries: build per-step StepResult.
			if stepSource, _ := chunk.Metadata["stepSource"].(string); stepSource == "goai" {
				ts.currentStep++
				// Response is set directly on the chunk by the step loop.
				// ProviderMetadata is embedded in Metadata (no dedicated StreamChunk field).
				var stepProviderMeta map[string]map[string]any
				if pm, ok := chunk.Metadata["providerMetadata"].(map[string]map[string]any); ok {
					stepProviderMeta = pm
				}
				ts.steps = append(ts.steps, StepResult{
					Number:           ts.currentStep,
					Text:             ts.stepText.String(),
					Reasoning:        ts.reasoningBuf.String(),
					ToolCalls:        ts.stepToolCalls,
					FinishReason:     chunk.FinishReason,
					Usage:            chunk.Usage,
					Sources:          ts.stepSources,
					Response:         chunk.Response,
					ProviderMetadata: stepProviderMeta,
				})
				// Accumulate per-step usage and finishReason for resilience:
				// if stream terminates before ChunkFinish (e.g., context cancel between
				// ChunkStepFinish and ChunkFinish sends), ts.usage still reflects completed steps.
				// ChunkFinish will overwrite with the authoritative totalUsage when it arrives.
				ts.usage = addUsage(ts.usage, chunk.Usage)
				ts.finishReason = chunk.FinishReason
				// Update ts.response for the overall TextResult (last step wins).
				ts.response = chunk.Response
				ts.providerMetadata = stepProviderMeta
				// Reset per-step accumulators.
				ts.stepText.Reset()
				ts.stepToolCalls = nil
				ts.stepSources = nil
				ts.reasoningBuf.Reset()
				ts.reasoningMeta = nil
			} else {
				// Provider-internal step boundary (e.g., Anthropic extended thinking).
				// Preserve existing behavior: extract response, metadata, sources.
				// This ensures single-step streaming continues to work correctly.
				// NOTE: Do NOT accumulate usage here with addUsage. Provider-internal
				// ChunkStepFinish usage is already included in the provider's ChunkFinish
				// (which uses direct assignment), and the GoAI ChunkStepFinish (which uses
				// addUsage via drainStep). Accumulating here would double-count.
				ts.finishReason = chunk.FinishReason
				ts.response = chunk.Response
				if sources, ok := chunk.Metadata["sources"].([]provider.Source); ok {
					ts.sources = append(ts.sources, sources...)
					ts.stepSources = append(ts.stepSources, sources...)
				}
				if pm, ok := chunk.Metadata["providerMetadata"].(map[string]map[string]any); ok {
					ts.providerMetadata = pm
				}
				// Copy flat metadata keys to Response.ProviderMetadata (existing behavior).
				for k, v := range chunk.Metadata {
					if k == "providerMetadata" || k == "sources" {
						continue
					}
					if ts.response.ProviderMetadata == nil {
						ts.response.ProviderMetadata = map[string]any{}
					}
					ts.response.ProviderMetadata[k] = v
				}
			}

		case provider.ChunkFinish:
			// Direct assignment (not addUsage): ChunkFinish carries authoritative total usage.
			ts.usage = chunk.Usage
			ts.finishReason = chunk.FinishReason
			// Preserve existing single-step behavior: extract response, metadata, sources
			// from the provider's ChunkFinish. For multi-step, the goai-emitted ChunkFinish
			// does not carry these (they are embedded in ChunkStepFinish metadata instead).
			if chunk.Response.ID != "" || chunk.Response.Model != "" {
				ts.response = chunk.Response
			}
			if sources, ok := chunk.Metadata["sources"].([]provider.Source); ok {
				ts.sources = append(ts.sources, sources...)
			}
			if pm, ok := chunk.Metadata["providerMetadata"].(map[string]map[string]any); ok {
				ts.providerMetadata = pm
			}
			// Copy flat metadata keys to Response.ProviderMetadata (existing behavior).
			for k, v := range chunk.Metadata {
				if k == "providerMetadata" || k == "sources" {
					continue
				}
				if ts.response.ProviderMetadata == nil {
					ts.response.ProviderMetadata = map[string]any{}
				}
				ts.response.ProviderMetadata[k] = v
			}

		case provider.ChunkError:
			if ts.streamErr == nil {
				ts.streamErr = chunk.Error
			}
		}

		if rawOut != nil {
			select {
			case rawOut <- chunk:
			case <-ts.ctx.Done():
				ts.streamErr = ts.ctx.Err()
				return
			}
		}
		if textOut != nil && chunk.Type == provider.ChunkText {
			select {
			case textOut <- chunk.Text:
			case <-ts.ctx.Done():
				ts.streamErr = ts.ctx.Err()
				return
			}
		}
		if rawOut == nil && textOut == nil {
			if ts.ctx.Err() != nil {
				ts.streamErr = ts.ctx.Err()
				return
			}
		}
	}
}

func (ts *TextStream) buildResult() *TextResult {
	text := ts.text.String() // full accumulated text across all steps
	result := &TextResult{
		Text:             text,
		ToolCalls:        ts.toolCalls,
		FinishReason:     ts.finishReason,
		TotalUsage:       ts.usage,
		Response:         ts.response,
		Sources:          ts.sources,
		ProviderMetadata: ts.providerMetadata,
	}
	if len(ts.steps) > 0 {
		result.Steps = ts.steps
		// Match GenerateText: ToolCalls is the LAST step's tool calls, not all steps'.
		result.ToolCalls = ts.steps[len(ts.steps)-1].ToolCalls
		// Aggregate per-step reasoning so streaming TextResult exposes
		// the same .Reasoning field as GenerateText.
		var reasoningAll strings.Builder
		for _, s := range ts.steps {
			reasoningAll.WriteString(s.Reasoning)
		}
		result.Reasoning = reasoningAll.String()
	} else if text != "" || len(ts.toolCalls) > 0 || ts.finishReason != "" {
		// Single-step fallback (no multi-step ChunkStepFinish received, but data exists).
		stepReasoning := ts.reasoningBuf.String()
		result.Steps = []StepResult{{
			Number:           1,
			Text:             ts.stepText.String(),
			Reasoning:        stepReasoning,
			ToolCalls:        ts.toolCalls,
			FinishReason:     ts.finishReason,
			Usage:            ts.usage,
			Response:         ts.response,
			Sources:          ts.sources,
			ProviderMetadata: ts.providerMetadata,
		}}
		result.Reasoning = stepReasoning
	}
	// No data: Steps is nil.

	// Set StepsExhausted from streamWithToolLoop goroutine.
	result.StepsExhausted = ts.stepsExhausted

	// Populate ResponseMessages.
	if ts.responseMessages != nil {
		// Multi-step: set by streamWithToolLoop goroutine.
		result.ResponseMessages = ts.responseMessages
	} else if text != "" || len(result.ToolCalls) > 0 {
		// Single-step: build a simple assistant message from the result.
		// Use stepText (text-only, excludes reasoning) for ResponseMessages so reasoning
		// doesn't get baked into PartText. Pass consolidated reasoning part separately
		// (matching drainStep: one Part with merged metadata including signatures).
		var reasoning []provider.Part
		if ts.reasoningBuf.Len() > 0 || len(ts.reasoningMeta) > 0 {
			reasoning = []provider.Part{{
				Type:            provider.PartReasoning,
				Text:            ts.reasoningBuf.String(),
				ProviderOptions: ts.reasoningMeta,
			}}
		}
		result.ResponseMessages = buildFinalAssistantMessages(ts.stepText.String(), result.ToolCalls, reasoning)
	}
	return result
}

type drainResult struct {
	text             string          // text-only (ChunkText), used for appendToolRoundTrip
	reasoning        []provider.Part // reasoning/thinking parts, echoed back for providers that require it (e.g. Bedrock)
	reasoningText    string          // consolidated reasoning text (PartReasoning), surfaced via StepResult.Reasoning
	toolCalls        []provider.ToolCall
	usage            provider.Usage
	finishReason     provider.FinishReason
	sources          []provider.Source
	response         provider.ResponseMetadata
	providerMetadata map[string]map[string]any
	err              error // non-nil if context cancelled during drain
}

func drainStep(
	ctx context.Context,
	source <-chan provider.StreamChunk,
	out chan<- provider.StreamChunk,
) drainResult {
	var (
		textBuf       strings.Builder // ChunkText only (reasoning excluded)
		reasoningBuf  strings.Builder // accumulated reasoning text
		reasoningMeta map[string]any  // last metadata (contains signature)
		dr            drainResult
	)

	for chunk := range source {
		// Forward chunk to consumer. Suppress these types (handled explicitly below):
		// - ChunkFinish: step loop emits its own ChunkFinish with totalUsage
		// - ChunkError: forwarded explicitly in the switch to avoid double-send
		// Provider ChunkStepFinish IS forwarded (not suppressed) so Stream() consumers
		// can see provider-internal boundaries (e.g., Anthropic thinking steps). These
		// do NOT carry Metadata["stepSource"]="goai", so consume() distinguishes them.
		if chunk.Type != provider.ChunkFinish && chunk.Type != provider.ChunkError {
			if !provider.TrySend(ctx, out, chunk) {
				// Drain source to unblock provider.
				drainRemaining(source)
				dr.err = ctx.Err()
				return dr
			}
		}

		// Accumulate state (same logic as TextStream.consume, generate.go:198-242).
		// Note: ChunkToolCallDelta, ChunkToolCallStreamStart, and ChunkToolResult are
		// forwarded to the consumer (not suppressed) but NOT accumulated here. drainStep
		// only captures complete ChunkToolCall chunks. Providers always emit a final
		// ChunkToolCall with complete data after all deltas.
		switch chunk.Type {
		case provider.ChunkText:
			textBuf.WriteString(chunk.Text)
			if s, ok := chunk.Metadata["source"].(provider.Source); ok {
				dr.sources = append(dr.sources, s)
			}
		case provider.ChunkReasoning:
			// Accumulate into a single buffer. The final chunk carries the
			// signature (text="", metadata={"signature":"..."}); earlier chunks
			// carry text fragments. Consolidating produces one complete part.
			if chunk.Text != "" {
				reasoningBuf.WriteString(chunk.Text)
			}
			if chunk.Metadata != nil {
				if reasoningMeta == nil {
					reasoningMeta = make(map[string]any)
				}
				for k, v := range chunk.Metadata {
					reasoningMeta[k] = v
				}
			}
			if s, ok := chunk.Metadata["source"].(provider.Source); ok {
				dr.sources = append(dr.sources, s)
			}
		case provider.ChunkToolCall:
			dr.toolCalls = append(dr.toolCalls, provider.ToolCall{
				ID:       chunk.ToolCallID,
				Name:     chunk.ToolName,
				Input:    json.RawMessage(chunk.ToolInput),
				Metadata: chunk.Metadata,
			})
		case provider.ChunkStepFinish:
			// Provider-internal step boundary (e.g., Anthropic extended thinking).
			// Use direct assignment (last value wins), matching ChunkFinish below.
			// This is correct for both zero-usage providers (Anthropic: 0 overwrites 0)
			// and running-total providers (Google: last total is authoritative).
			dr.finishReason = chunk.FinishReason
			dr.usage = chunk.Usage
			dr.response = chunk.Response
			if sources, ok := chunk.Metadata["sources"].([]provider.Source); ok {
				dr.sources = append(dr.sources, sources...)
			}
			if pm, ok := chunk.Metadata["providerMetadata"].(map[string]map[string]any); ok {
				dr.providerMetadata = pm
			}
			for k, v := range chunk.Metadata {
				if k == "providerMetadata" || k == "sources" {
					continue
				}
				if dr.response.ProviderMetadata == nil {
					dr.response.ProviderMetadata = map[string]any{}
				}
				dr.response.ProviderMetadata[k] = v
			}
		case provider.ChunkFinish:
			// Terminal chunk. Use direct assignment for usage (not addUsage) to avoid
			// double-counting when providers emit both ChunkStepFinish and ChunkFinish
			// with the same accumulated usage (e.g., Google).
			dr.finishReason = chunk.FinishReason
			dr.usage = chunk.Usage
			dr.response = chunk.Response
			if sources, ok := chunk.Metadata["sources"].([]provider.Source); ok {
				dr.sources = append(dr.sources, sources...)
			}
			if pm, ok := chunk.Metadata["providerMetadata"].(map[string]map[string]any); ok {
				dr.providerMetadata = pm
			}
			// Copy flat metadata keys to Response.ProviderMetadata (same as consume(),
			// generate.go:229-237). Providers use this for per-response data: Anthropic
			// ("iterations", "contextManagement"), Bedrock ("cacheWriteInputTokens").
			for k, v := range chunk.Metadata {
				if k == "providerMetadata" || k == "sources" {
					continue
				}
				if dr.response.ProviderMetadata == nil {
					dr.response.ProviderMetadata = map[string]any{}
				}
				dr.response.ProviderMetadata[k] = v
			}
		case provider.ChunkError:
			// Forward error chunks to consumer. Mid-stream errors flow through
			// ChunkError chunks to the consumer; OnResponse does not report them.
			if !provider.TrySend(ctx, out, chunk) {
				drainRemaining(source)
				dr.err = ctx.Err()
				return dr
			}
		}
	}

	dr.text = textBuf.String()
	dr.reasoningText = reasoningBuf.String()

	// Consolidate reasoning fragments into one Part (text + signature metadata)
	// so the codec serializes a single complete block.
	if reasoningBuf.Len() > 0 || len(reasoningMeta) > 0 {
		dr.reasoning = []provider.Part{{
			Type:            provider.PartReasoning,
			Text:            reasoningBuf.String(),
			ProviderOptions: reasoningMeta,
		}}
	}

	return dr
}

// drainRemaining reads and discards all remaining chunks from source.
// This unblocks the provider's write-side goroutine on context cancellation,
// preventing goroutine leaks. Note: this blocks until the provider closes its
// channel. A misbehaving provider that never closes will cause this to hang.
// All current providers use defer close(out) so this is safe in practice.
func drainRemaining(source <-chan provider.StreamChunk) {
	for range source {
		// discard
	}
}
