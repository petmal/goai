package goai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/zendev-sh/goai/provider"
)

// ErrUnknownTool is returned when a tool call references a tool not in the tool map.
var ErrUnknownTool = errors.New("goai: unknown tool")

// toolCallIDKey is the context key for the current tool call ID.
type toolCallIDKey struct{}

// ToolCallIDFromContext returns the tool call ID from the context.
// This is available inside a Tool's Execute function to identify which
// tool call is being executed.
func ToolCallIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(toolCallIDKey{}).(string); ok {
		return v
	}
	return ""
}

// TextResult is the final result of a text generation call.
type TextResult struct {
	// Text is the accumulated generated text across all steps.
	// For StreamText, this includes reasoning tokens (ChunkReasoning) for backward
	// compatibility. Use Steps[n].Text for text-only content excluding reasoning.
	Text string

	// Reasoning is the model's accumulated thinking/reasoning text
	// across all steps (PartReasoning). Populated for both GenerateText
	// and StreamText when the provider returns reasoning content (e.g.
	// Anthropic extended thinking on Bedrock). Empty when reasoning is
	// disabled or unsupported.
	Reasoning string

	// ToolCalls requested by the model in the final step.
	ToolCalls []provider.ToolCall

	// Steps contains results from each generation step (for multi-step tool loops).
	Steps []StepResult

	// TotalUsage is the aggregated token usage across all steps.
	TotalUsage provider.Usage

	// FinishReason indicates why generation stopped.
	FinishReason provider.FinishReason

	// Response contains provider metadata from the last step (ID, Model).
	Response provider.ResponseMetadata

	// ProviderMetadata contains provider-specific response data from the last step
	// (e.g. logprobs, prediction tokens).
	ProviderMetadata map[string]map[string]any

	// Sources contains citations/references extracted from the response.
	Sources []provider.Source

	// StepsExhausted is true when the tool loop terminated because MaxSteps was reached
	// while the model still requested tool calls. This distinguishes "model finished
	// naturally" (StepsExhausted=false) from "loop was cut short" (StepsExhausted=true).
	StepsExhausted bool

	// ResponseMessages contains the assistant and tool messages from all generation steps.
	// For multi-turn conversations, append these to your message history:
	//   messages = append(messages, result.ResponseMessages...)
	//
	// Nil when the response has no content (empty text and no tool calls).
	// For StreamText, check Err() before using , on stream errors, ResponseMessages
	// may be partial (intermediate tool round-trips lost) or reflect only completed
	// steps. Do not use ResponseMessages for conversation continuation when Err() != nil.
	// Reasoning parts (PartReasoning) are included for StreamText (both single-step
	// and multi-step) but not for GenerateText (which does not expose reasoning).
	// Reasoning chunks are consolidated into a single PartReasoning part with merged
	// metadata (e.g. Anthropic/Bedrock signatures).
	ResponseMessages []provider.Message
}

// StepResult is the result of a single generation step in a tool loop.
type StepResult struct {
	// Number is the 1-based step index.
	Number int

	// Text generated in this step (excludes reasoning tokens).
	// For StreamText, reasoning is included in TextResult.Text but excluded here.
	Text string

	// Reasoning is the consolidated thinking/reasoning text for this
	// step (PartReasoning, signature stripped). Populated for both
	// GenerateText and StreamText when the provider returns reasoning.
	Reasoning string

	// ToolCalls requested in this step.
	ToolCalls []provider.ToolCall

	// ToolResults contains one entry per completed ToolCall in this step,
	// populated AFTER executeToolsParallel returns and BEFORE WithStopWhen
	// is evaluated. Ordering matches ToolCalls element-for-element.
	//
	// Empty when the step had no tool calls or when the loop exits before
	// executing tools (e.g. MaxSteps reached with pending tool calls that
	// never ran, or StopCauseNoExecutableTools).
	//
	// Mirrors Vercel AI SDK's DefaultStepResult.toolResults so predicates
	// passed to WithStopWhen can inspect tool outputs (matching the
	// placement documented on StopCondition).
	//
	// Streaming visibility: consumers using StreamText who read raw
	// chunks via stream.Stream() cannot observe per-step ToolResults in
	// real time. The ChunkStepFinish chunk with stepSource="goai" is
	// emitted BEFORE tools execute (so ToolResults is empty at that
	// point); the subsequent goai-internal "goai-tool-results" chunk
	// that backfills ToolResults is consumed by the stream reducer and
	// NOT re-emitted to the raw chunk channel. To observe ToolResults
	// per step from a streaming call, use one of:
	//   - stream.Result() after the stream closes (Steps[].ToolResults
	//     is fully populated).
	//   - OnToolCall hook (fires synchronously after each tool Execute
	//     returns with per-call detail).
	//   - OnAfterToolExecute hook (same timing as OnToolCall, richer
	//     metadata).
	// The OnStepFinish hook always receives a StepResult with an EMPTY
	// ToolResults slice because tools execute AFTER the hook fires.
	ToolResults []provider.ToolResult

	// FinishReason for this step.
	FinishReason provider.FinishReason

	// Usage for this step.
	Usage provider.Usage

	// Response contains provider metadata for this step (ID, Model).
	Response provider.ResponseMetadata

	// ProviderMetadata contains provider-specific response data for this step.
	ProviderMetadata map[string]map[string]any

	// Sources contains citations/references from this step.
	Sources []provider.Source
}

// buildParams converts options to provider.GenerateParams.
func buildParams(opts options) provider.GenerateParams {
	var tools []provider.ToolDefinition
	for _, t := range opts.Tools {
		tools = append(tools, provider.ToolDefinition{
			Name:                   t.Name,
			Description:            t.Description,
			InputSchema:            t.InputSchema,
			ProviderDefinedType:    t.ProviderDefinedType,
			ProviderDefinedOptions: t.ProviderDefinedOptions,
		})
	}

	msgs := opts.Messages
	if opts.Prompt != "" {
		msgs = append([]provider.Message{UserMessage(opts.Prompt)}, msgs...)
	} else {
		// Always copy so tool-loop appends never mutate the caller's slice.
		msgs = slices.Clone(msgs)
	}

	if opts.PromptCaching {
		msgs = applyCaching(msgs)
	}

	return provider.GenerateParams{
		Messages:         msgs,
		System:           opts.System,
		Tools:            tools,
		MaxOutputTokens:  opts.MaxOutputTokens,
		Temperature:      opts.Temperature,
		TopP:             opts.TopP,
		TopK:             opts.TopK,
		FrequencyPenalty: opts.FrequencyPenalty,
		PresencePenalty:  opts.PresencePenalty,
		Seed:             opts.Seed,
		StopSequences:    slices.Clone(opts.StopSequences),
		Headers:          maps.Clone(opts.Headers),
		ProviderOptions:  maps.Clone(opts.ProviderOptions),
		PromptCaching:    opts.PromptCaching,
		ToolChoice:       opts.ToolChoice,
	}
}

func streamWithToolLoop(ctx context.Context, model provider.LanguageModel, o options, toolMap map[string]Tool) (_ *TextStream, err error) {
	params := buildParams(o)
	originalLen := len(params.Messages)

	// Convert a *PanicError from a synchronous step-1 hook (OnRequest, or
	// OnResponse on the initial DoStream error) into the returned error. Hooks
	// fired inside the streaming goroutine surface through stream.Err().
	defer recoverToError(o.OnPanic, &err)

	var timeoutCancel context.CancelFunc
	if o.Timeout > 0 {
		ctx, timeoutCancel = context.WithTimeout(ctx, o.Timeout)
	}

	// AgentState: initial. o.StateRef may be nil (set is a no-op).
	o.StateRef.set(StepStarting, 0)

	// --- Step 1 DoStream: synchronous (preserves (nil, error) contract) ---
	// This ensures StreamText ALWAYS returns (nil, error) when the first DoStream
	// fails, regardless of MaxSteps. Eliminates the split error contract.
	o.StateRef.set(StepLLMInFlight, 1)
	for _, fn := range o.OnRequest {
		callHook(o.OnPanic, "OnRequest", func() {
			fn(RequestInfo{
				Ctx:          ctx,
				Model:        model.ModelID(),
				MessageCount: len(params.Messages),
				ToolCount:    len(params.Tools),
				Timestamp:    time.Now(),
				Messages:     requestMessages(params.System, params.Messages),
			})
		})
	}

	start := time.Now()
	firstResult, streamErr := withRetry(ctx, o.MaxRetries, o.RetryObserver, func() (*provider.StreamResult, error) {
		return model.DoStream(ctx, params)
	})
	if streamErr != nil {
		if timeoutCancel != nil {
			timeoutCancel()
		}
		// FIX 47: preserve step=1 on error - monotonicity. The store above
		// already moved the step counter to 1 (StepLLMInFlight, 1); a poller
		// observing between the two stores must not see step regress to 0.
		o.StateRef.set(StepIdle, 1)
		// Step-1 OnResponse runs in the caller's goroutine; callHook surfaces a
		// panic as a *PanicError via the deferred recoverToError above.
		for _, fn := range o.OnResponse {
			info := ResponseInfo{Latency: time.Since(start), Error: streamErr}
			var apiErr *APIError
			if errors.As(streamErr, &apiErr) {
				info.StatusCode = apiErr.StatusCode
			}
			callHook(o.OnPanic, "OnResponse", func() { fn(info) })
		}
		return nil, streamErr // SAME error contract as single-step StreamText
	}

	// Step 1 succeeded. Goroutine-local copy of start time avoids closure capture.
	step1Start := start
	out := make(chan provider.StreamChunk, 64)
	ts := newTextStream(ctx, out)

	go func() {
		defer close(out)
		// Surface a panic from any inline-fired hook (OnBeforeStep, OnRequest,
		// OnResponse, OnStepFinish) or the StopWhen predicate as a *PanicError
		// on the stream: emit it as a ChunkError (consume sets stream.Err() from
		// it) followed by a ChunkFinish so the stream terminates cleanly. The
		// OnPanic observers already fired via callHook for a *PanicError; a raw
		// runtime panic is wrapped here so the goroutine never crashes.
		defer func() {
			if r := recover(); r != nil {
				pe := asPanicError(r)
				if pe == nil {
					pe = newPanicError(o.OnPanic, "stream", r)
				}
				provider.TrySend(ctx, out, provider.StreamChunk{Type: provider.ChunkError, Error: pe})
				provider.TrySend(ctx, out, provider.StreamChunk{Type: provider.ChunkFinish, StoppedBy: provider.StopCauseAbort})
			}
		}()
		if timeoutCancel != nil {
			defer timeoutCancel()
		}
		var totalUsage provider.Usage
		var lastFinishReason provider.FinishReason
		var lastResponse provider.ResponseMetadata
		var lastReasoning []provider.Part
		var steps []StepResult
		var stepsExhausted bool
		var hookStopped bool             // true iff WithStopWhen or OnBeforeStep.Stop broke the loop
		var stopCause provider.StopCause // classifies how the loop exited (FIX 5)
		firstStep := true                // true only for step 1 (already have firstResult)
		stepStart := step1Start          // goroutine-local start time per step

		// highestInflightStep tracks the maximum step counter announced via
		// o.StateRef.set(StepLLMInFlight, ...). The deferred StepIdle publish
		// uses max(len(steps), highestInflightStep) so the observable step
		// value never regresses (FIX 47 monotonicity: if a mid-loop step
		// errors before being appended to `steps`, len(steps) lags behind
		// highestInflightStep; the defer must publish the larger value).
		//
		// There are TWO writes to highestInflightStep in this function, and
		// both are intentional (FIX 54 + FIX 55):
		//
		//   1. The `= 1` assignment below (pre-loop): mirrors the
		//      o.StateRef.set(StepLLMInFlight, 1) call that happens BEFORE
		//      this goroutine starts (see the pre-goroutine set call
		//      earlier in streamWithToolLoop). This guarantees the deferred
		//      StepIdle publish has something >= 1 to report even if the
		//      goroutine exits before entering the loop body (e.g. panic
		//      recovery, pre-loop early exit).
		//   2. The `= step` assignment in the firstStep branch (loop body):
		//      refactor-safety for future changes that move the step-1
		//      StepLLMInFlight announcement inside the loop. If such a
		//      refactor happens, write #1 should be deleted; write #2
		//      continues to maintain the invariant from inside the loop.
		//
		// Both writes together ensure the invariant "highestInflightStep
		// reflects the latest StepLLMInFlight announcement" holds on every
		// path the defer can fire from.
		highestInflightStep := 1
		// Ensure any exit path (break, return, panic-recover-above, natural
		// termination) leaves the observable state as Idle. Use closure-captured
		// steps so the final step count is visible to pollers.
		defer func() {
			finalStep := len(steps)
			if highestInflightStep > finalStep {
				finalStep = highestInflightStep
			}
			o.StateRef.set(StepIdle, finalStep)
		}()

		for step := 1; step <= o.MaxSteps; step++ {
			var result *provider.StreamResult

			if firstStep {
				// Step 1: use the already-obtained firstResult.
				result = firstResult
				firstStep = false
				// FIX 54: record the step-1 announcement inside the loop body
				// so the invariant "every StepLLMInFlight has a matching
				// highestInflightStep write" holds from step 1. The actual
				// atomic store happened before the goroutine; this is the
				// in-loop bookkeeping companion.
				highestInflightStep = step
			} else {
				// Steps 2+: OnBeforeStep hook (can inject messages or stop loop).
				if o.OnBeforeStep != nil {
					var bsr BeforeStepResult
					callHook(o.OnPanic, "OnBeforeStep", func() {
						bsr = o.OnBeforeStep(BeforeStepInfo{
							Ctx:      ctx,
							Step:     step,
							Messages: slices.Clone(params.Messages),
						})
					})
					if bsr.Stop {
						// Semantic parity with WithStopWhen: mark as hookStopped.
						hookStopped = true
						stopCause = provider.StopCauseBeforeStep
						break
					}
					if len(bsr.ExtraMessages) > 0 {
						params.Messages = append(params.Messages, bsr.ExtraMessages...)
					}
				}

				// Steps 2+: DoStream inside goroutine.
				for _, fn := range o.OnRequest {
					callHook(o.OnPanic, "OnRequest", func() {
						fn(RequestInfo{
							Ctx:          ctx,
							Model:        model.ModelID(),
							MessageCount: len(params.Messages),
							ToolCount:    len(params.Tools),
							Timestamp:    time.Now(),
							Messages:     requestMessages(params.System, params.Messages),
						})
					})
				}

				stepStart = time.Now()
				o.StateRef.set(StepLLMInFlight, step)
				highestInflightStep = step
				var err error
				result, err = withRetry(ctx, o.MaxRetries, o.RetryObserver, func() (*provider.StreamResult, error) {
					return model.DoStream(ctx, params)
				})
				if err != nil {
					// Fire OnResponse on error.
					for _, fn := range o.OnResponse {
						info := ResponseInfo{Latency: time.Since(stepStart), Error: err}
						var apiErr *APIError
						if errors.As(err, &apiErr) {
							info.StatusCode = apiErr.StatusCode
						}
						callHook(o.OnPanic, "OnResponse", func() { fn(info) })
					}
					// responseMessages intentionally not set on error , buildResult falls back to
					// a minimal assistant message from accumulated text. Intermediate tool round-trip
					// messages are lost. Callers should check Err() and not rely on ResponseMessages
					// when the stream has errors.
					provider.TrySend(ctx, out, provider.StreamChunk{Type: provider.ChunkError, Error: err})
					provider.TrySend(ctx, out, provider.StreamChunk{Type: provider.ChunkFinish, FinishReason: lastFinishReason, Usage: totalUsage, StoppedBy: provider.StopCauseAbort})
					// Fire OnFinish so observability hooks can close spans/flush traces.
					lastFinish := provider.FinishReason("")
					if len(steps) > 0 {
						lastFinish = steps[len(steps)-1].FinishReason
					}
					fireOnFinish(o.OnPanic, o.OnFinish, FinishInfo{
						TotalSteps:   len(steps),
						TotalUsage:   totalUsage,
						FinishReason: lastFinish,
						StoppedBy:    provider.StopCauseAbort,
					})
					return
				}
			}

			ds := drainStep(ctx, result.Stream, out)
			if ds.err != nil {
				// responseMessages intentionally not set on error - buildResult falls back to
				// a minimal assistant message from accumulated text. Intermediate tool round-trip
				// messages are lost. Callers should check Err() and not rely on ResponseMessages
				// when the stream has errors.
				provider.TrySend(ctx, out, provider.StreamChunk{Type: provider.ChunkError, Error: ds.err})
				provider.TrySend(ctx, out, provider.StreamChunk{Type: provider.ChunkFinish, Usage: totalUsage, StoppedBy: provider.StopCauseAbort})
				// Fire OnFinish so observability hooks can close spans/flush traces.
				// Without this, OTel root spans leak and Langfuse traces are lost.
				lastFinish := provider.FinishReason("")
				if len(steps) > 0 {
					lastFinish = steps[len(steps)-1].FinishReason
				}
				fireOnFinish(o.OnPanic, o.OnFinish, FinishInfo{
					TotalSteps:   len(steps),
					TotalUsage:   totalUsage,
					FinishReason: lastFinish,
					StoppedBy:    provider.StopCauseAbort,
				})
				return
			}

			// Guard: skip empty step (provider closed channel without sending
			// any meaningful chunks, e.g., after a ChunkError). Prevents emitting
			// a phantom empty StepResult and ChunkStepFinish. This is not an
			// error path (a separate ChunkError path covers real errors) - use
			// StopCauseEmpty so consumers can distinguish a no-op response from
			// an abort.
			if ds.text == "" && len(ds.toolCalls) == 0 && ds.finishReason == "" {
				stopCause = provider.StopCauseEmpty
				break
			}

			// OnResponse: Error is NOT set (call succeeded). Mid-stream errors use stream.Err().
			for _, fn := range o.OnResponse {
				callHook(o.OnPanic, "OnResponse", func() {
					fn(ResponseInfo{
						Latency:      time.Since(stepStart),
						Usage:        ds.usage,
						FinishReason: ds.finishReason,
					})
				})
			}

			// --- Build StepResult, fire OnStepFinish ---
			stepResult := StepResult{
				Number:           step,
				Text:             ds.text,
				Reasoning:        ds.reasoningText,
				ToolCalls:        ds.toolCalls,
				FinishReason:     ds.finishReason,
				Usage:            ds.usage,
				Sources:          ds.sources,
				Response:         ds.response,
				ProviderMetadata: ds.providerMetadata,
			}
			steps = append(steps, stepResult)
			totalUsage = addUsage(totalUsage, ds.usage)
			lastResponse = ds.response
			lastReasoning = ds.reasoning

			// AgentState: the step's stream has fully drained; tool exec and
			// stop-predicate evaluation have not started yet. Pollers observing
			// in this window must see StepStepFinished, not StepLLMInFlight.
			o.StateRef.set(StepStepFinished, step)

			// OnStepFinish.
			for _, fn := range o.OnStepFinish {
				callHook(o.OnPanic, "OnStepFinish", func() { fn(stepResult) })
			}

			// Normalize: providers that send empty/wrong finish_reason with tool calls
			// (MiniMax, Azure MaaS deepseek, etc.) would cause the loop to exit early.
			// The presence of tool calls is authoritative. Must run BEFORE capturing
			// lastFinishReason so the final ChunkFinish carries a non-empty reason
			// even when providers (gemini, bedrock-sonnet, azure-sonnet) emit empty
			// finish_reason alongside tool calls.
			if len(ds.toolCalls) > 0 && ds.finishReason != provider.FinishToolCalls {
				ds.finishReason = provider.FinishToolCalls
			}
			lastFinishReason = ds.finishReason

			// --- Emit ChunkStepFinish ---
			// Set Response directly on the chunk (StreamChunk.Response is a plain struct
			// field with no restriction on who sets it). ProviderMetadata goes in Metadata
			// since there is no dedicated field for it on StreamChunk.
			provider.TrySend(ctx, out, provider.StreamChunk{
				Type:         provider.ChunkStepFinish,
				FinishReason: ds.finishReason,
				Usage:        ds.usage,
				Response:     ds.response,
				Metadata: map[string]any{
					"stepSource":       "goai",
					"providerMetadata": ds.providerMetadata,
				},
			})

			// --- Exit conditions (same as GenerateText) ---
			// Note: streamWithToolLoop is only entered when len(toolMap) > 0
			// (guarded at StreamText entry - see generate.go:992), and toolMap
			// is immutable for the lifetime of the call. The "no executable
			// tools" exit therefore cannot be reached here; StopCauseNoExecutableTools
			// is a sync-only cause (GenerateText). See StopCauseNoExecutableTools
			// godoc in provider/types.go.
			if ds.finishReason != provider.FinishToolCalls || len(ds.toolCalls) == 0 {
				stopCause = provider.StopCauseNatural
				break
			}

			// --- Execute tools in parallel ---
			o.StateRef.set(StepToolExecuting, step)
			toolMsgs, toolResults := executeToolsParallel(ctx, ds.toolCalls, toolMap, step, toolHooks{
				sequential:      o.SequentialTools,
				onToolCallStart: o.OnToolCallStart,
				onToolCall:      o.OnToolCall,
				onBeforeExecute: o.OnBeforeToolExecute,
				onAfterExecute:  o.OnAfterToolExecute,
				onPanic:         o.OnPanic,
			})
			// Attach ToolResults to the step BEFORE the stop predicate sees it
			// (FIX 7 / Vercel DefaultStepResult parity). steps[-1] is this step.
			steps[len(steps)-1].ToolResults = toolResults

			// Notify the consumer (TextStream.consume) that this step now has
			// tool results so ts.steps[len-1].ToolResults can be backfilled.
			// We reuse ChunkStepFinish with a distinguishing stepSource tag so
			// existing provider-internal ChunkStepFinish handling is untouched.
			provider.TrySend(ctx, out, provider.StreamChunk{
				Type: provider.ChunkStepFinish,
				Metadata: map[string]any{
					"stepSource":  "goai-tool-results",
					"toolResults": toolResults,
				},
			})

			// --- Append messages for next step ---
			params.Messages = appendToolRoundTrip(params.Messages, ds.text, ds.reasoning, ds.toolCalls, toolMsgs)
			// Clear ToolChoice so model can freely respond on subsequent steps.
			// Set on every iteration for simplicity; idempotent after step 1.
			params.ToolChoice = ""

			// WithStopWhen (Vercel parity): evaluated AFTER this step's tool
			// executions complete and the tool-result messages have been
			// folded into params.Messages. ResponseMessages built on break
			// here is a valid replay transcript (matching tool_use /
			// tool_result pairs). Matches
			// vercel-ai/packages/ai/src/generate-text/generate-text.ts.
			if o.StopWhen != nil && stopSafe(o.OnPanic, o.StopWhen, steps) {
				hookStopped = true
				stopCause = provider.StopCausePredicate
				break
			}
		}

		stopCause, stepsExhausted = finalizeStopCause(hookStopped, stopCause, steps, o.MaxSteps)

		// Set responseMessages before emitting ChunkFinish (safe: ts.buildResult reads
		// responseMessages only after doneCh closes, which happens after out is closed).
		ts.stepsExhausted = stepsExhausted
		ts.responseMessages = buildResponseMessages(params.Messages[originalLen:], steps, lastReasoning)

		fireOnFinish(o.OnPanic, o.OnFinish, FinishInfo{
			StepsExhausted: stepsExhausted,
			TotalSteps:     len(steps),
			TotalUsage:     totalUsage,
			FinishReason:   lastFinishReason,
			StoppedBy:      stopCause,
		})

		// Emit final ChunkFinish with total usage and last step Response metadata.
		provider.TrySend(ctx, out, provider.StreamChunk{
			Type:         provider.ChunkFinish,
			FinishReason: lastFinishReason,
			Usage:        totalUsage,
			Response:     lastResponse,
			StoppedBy:    stopCause,
		})
	}()

	// OnResponse handled per-step inside the goroutine (ts.onResponse not set).
	return ts, nil
}

// StreamText performs a streaming text generation.
// When MaxSteps > 1 and executable tools are provided, StreamText runs an automatic
// tool loop. The initial DoStream failure still returns (nil, error). Subsequent step
// errors flow through the stream as ChunkError chunks; check stream.Err() after consuming.
func StreamText(ctx context.Context, model provider.LanguageModel, opts ...Option) (_ *TextStream, err error) {
	// Apply options FIRST so o.StateRef is populated before any early return.
	// This guarantees pollers waiting for StepIdle do not deadlock when we
	// return (nil, err) before the streaming goroutine starts (e.g. nil model,
	// empty prompt, initial DoStream failure).
	o := applyOptions(opts...)

	// Convert a *PanicError from a synchronous step-1 hook (OnRequest, or
	// OnResponse on a DoStream error) into the returned error. Hooks fired in
	// the consume goroutine surface through stream.Err() instead.
	defer recoverToError(o.OnPanic, &err)

	if model == nil {
		// Transition StepStarting→StepIdle for any observer so pollers do not deadlock.
		o.StateRef.set(StepStarting, 0)
		o.StateRef.set(StepIdle, 0)
		return nil, errors.New("goai: model must not be nil")
	}

	if o.Prompt == "" && len(o.Messages) == 0 {
		// Pre-loop validation error must still transition any observer to
		// StepIdle so pollers waiting on it do not deadlock.
		o.StateRef.set(StepStarting, 0)
		o.StateRef.set(StepIdle, 0)
		return nil, errors.New("goai: prompt or messages must not be empty")
	}

	toolMap, err := buildToolMap(o.Tools)
	if err != nil {
		// Pre-loop validation error must still transition any observer
		// StepStarting->StepIdle so pollers waiting on it do not deadlock,
		// consistent with the nil-model and empty-prompt checks above.
		o.StateRef.set(StepStarting, 0)
		o.StateRef.set(StepIdle, 0)
		return nil, err
	}

	if o.MaxSteps > 1 && len(toolMap) > 0 {
		return streamWithToolLoop(ctx, model, o, toolMap)
	}

	var timeoutCancel context.CancelFunc
	if o.Timeout > 0 {
		ctx, timeoutCancel = context.WithTimeout(ctx, o.Timeout)
	}

	params := buildParams(o)

	// FIX 34: single-shot StreamText never touched StateRef. Pollers using
	// WithStateRef(&state) + WithMaxSteps(1) (or no executable tools) got
	// stuck at the zero value (StepStarting, 0) forever. Transition through
	// StepStarting → StepLLMInFlight here; StepIdle is deferred in consume
	// (see ts.stateRef assignment below).
	o.StateRef.set(StepStarting, 0)
	o.StateRef.set(StepLLMInFlight, 1)

	for _, fn := range o.OnRequest {
		callHook(o.OnPanic, "OnRequest", func() {
			fn(RequestInfo{
				Ctx:          ctx,
				Model:        model.ModelID(),
				MessageCount: len(params.Messages),
				ToolCount:    len(params.Tools),
				Timestamp:    time.Now(),
				Messages:     requestMessages(params.System, params.Messages),
			})
		})
	}

	start := time.Now()
	result, streamErr := withRetry(ctx, o.MaxRetries, o.RetryObserver, func() (*provider.StreamResult, error) {
		return model.DoStream(ctx, params)
	})
	if streamErr != nil {
		if timeoutCancel != nil {
			timeoutCancel()
		}
		// FIX 34: DoStream failed before the consume goroutine could be
		// started, so the consume-based StepIdle defer will never run.
		// Transition to StepIdle inline so pollers waiting for it do not
		// deadlock. FIX 47: preserve step=1 (the step we just set to
		// LLMInFlight) instead of regressing to 0 - pollers observing
		// between the StepLLMInFlight store above and this store must
		// see a monotonically non-decreasing step counter.
		o.StateRef.set(StepIdle, 1)
		for _, fn := range o.OnResponse {
			info := ResponseInfo{Latency: time.Since(start), Error: streamErr}
			var apiErr *APIError
			if errors.As(streamErr, &apiErr) {
				info.StatusCode = apiErr.StatusCode
			}
			callHook(o.OnPanic, "OnResponse", func() { fn(info) })
		}
		return nil, streamErr
	}

	ts := newTextStream(ctx, result.Stream)
	ts.timeoutCancel = timeoutCancel
	ts.onResponse = o.OnResponse
	ts.onStepFinish = o.OnStepFinish
	ts.onFinish = o.OnFinish
	ts.onPanic = o.OnPanic
	ts.startTime = start
	// FIX 34: hand StateRef ownership to the consume goroutine; it will
	// transition to StepIdle when the stream drains / errors. Only set on
	// the single-shot path; streamWithToolLoop manages StateRef inline.
	ts.stateRef = o.StateRef
	return ts, nil
}

// GenerateText performs a non-streaming text generation.
// When tools with Execute functions are provided and MaxSteps > 1,
// it automatically runs a tool loop: generate → execute tools → re-generate.
func GenerateText(ctx context.Context, model provider.LanguageModel, opts ...Option) (_ *TextResult, err error) {
	// Apply options FIRST so o.StateRef is populated before any early return.
	// Registered BEFORE nil-model / prompt validation so pre-loop error returns
	// also transition observers to StepIdle (otherwise pollers waiting for
	// StepIdle would deadlock on validation errors).
	o := applyOptions(opts...)

	// Convert a *PanicError raised by a panicking hook or the StopWhen predicate
	// into the returned error. Registered first so it runs last (after the
	// StepIdle transition below), catching panics from the whole body.
	defer recoverToError(o.OnPanic, &err)

	var steps []StepResult
	// highestInflightStep tracks the largest step index we have announced
	// via StepLLMInFlight. Used by the StepIdle defer to enforce
	// monotonicity (FIX 47): if step N's DoGenerate errors before the
	// step is appended to `steps`, len(steps) is N-1 but we already
	// advertised StepLLMInFlight at N, so the final StepIdle must carry
	// max(len(steps), highestInflightStep) to avoid a step-counter
	// regression visible to pollers.
	highestInflightStep := 1
	// AgentState: initial (StepStarting, 0). set() is a no-op when o.StateRef is nil.
	o.StateRef.set(StepStarting, 0)
	defer func() {
		finalStep := len(steps)
		if highestInflightStep > finalStep {
			finalStep = highestInflightStep
		}
		o.StateRef.set(StepIdle, finalStep)
	}()

	if model == nil {
		return nil, errors.New("goai: model must not be nil")
	}

	if o.Prompt == "" && len(o.Messages) == 0 {
		return nil, errors.New("goai: prompt or messages must not be empty")
	}

	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}

	params := buildParams(o)
	originalLen := len(params.Messages)

	// Build tool lookup for auto loop.
	toolMap, err := buildToolMap(o.Tools)
	if err != nil {
		return nil, err
	}

	var totalUsage provider.Usage
	var hookStopped bool             // true iff WithStopWhen or OnBeforeStep.Stop broke the loop
	var stopCause provider.StopCause // classifies how the loop exited (FIX 5)

	for step := 1; step <= o.MaxSteps; step++ {
		// OnBeforeStep: step 2+ only (step 1 has no prior tool results to act on).
		if step > 1 && o.OnBeforeStep != nil {
			var bsr BeforeStepResult
			callHook(o.OnPanic, "OnBeforeStep", func() {
				bsr = o.OnBeforeStep(BeforeStepInfo{
					Ctx:      ctx,
					Step:     step,
					Messages: slices.Clone(params.Messages),
				})
			})
			if bsr.Stop {
				// Semantic parity with WithStopWhen: mark as hookStopped so
				// post-loop StepsExhausted derivation does not mistake a
				// hook-driven break at the MaxSteps boundary for natural exhaustion.
				hookStopped = true
				stopCause = provider.StopCauseBeforeStep
				break
			}
			if len(bsr.ExtraMessages) > 0 {
				params.Messages = append(params.Messages, bsr.ExtraMessages...)
			}
		}

		for _, fn := range o.OnRequest {
			callHook(o.OnPanic, "OnRequest", func() {
				fn(RequestInfo{
					Ctx:          ctx,
					Model:        model.ModelID(),
					MessageCount: len(params.Messages),
					ToolCount:    len(params.Tools),
					Timestamp:    time.Now(),
					Messages:     requestMessages(params.System, params.Messages),
				})
			})
		}

		start := time.Now()
		o.StateRef.set(StepLLMInFlight, step)
		highestInflightStep = step
		result, err := withRetry(ctx, o.MaxRetries, o.RetryObserver, func() (*provider.GenerateResult, error) {
			return model.DoGenerate(ctx, params)
		})

		for _, fn := range o.OnResponse {
			info := ResponseInfo{Latency: time.Since(start), Error: err}
			if err == nil {
				info.Usage = result.Usage
				info.FinishReason = result.FinishReason
			}
			var apiErr *APIError
			if errors.As(err, &apiErr) {
				info.StatusCode = apiErr.StatusCode
			}
			callHook(o.OnPanic, "OnResponse", func() { fn(info) })
		}

		if err != nil {
			// Fire OnFinish so user hooks can observe error termination.
			// Observability hooks (OTel/Langfuse) already handle this via OnResponse
			// error -> end(), but user-registered OnFinish hooks need this signal.
			lastFinish := provider.FinishReason("")
			if len(steps) > 0 {
				lastFinish = steps[len(steps)-1].FinishReason
			}
			fireOnFinish(o.OnPanic, o.OnFinish, FinishInfo{
				TotalSteps:   len(steps),
				TotalUsage:   totalUsage,
				FinishReason: lastFinish,
				StoppedBy:    provider.StopCauseAbort,
			})
			return nil, err
		}

		stepResult := StepResult{
			Number:           step,
			Text:             result.Text,
			Reasoning:        result.Reasoning,
			ToolCalls:        result.ToolCalls,
			FinishReason:     result.FinishReason,
			Usage:            result.Usage,
			Response:         result.Response,
			ProviderMetadata: result.ProviderMetadata,
			Sources:          result.Sources,
		}
		steps = append(steps, stepResult)
		totalUsage = addUsage(totalUsage, result.Usage)

		// AgentState: LLM call for this step is complete; tool exec and
		// stop-predicate evaluation have not started yet. Pollers observing
		// in this window must see StepStepFinished, not StepLLMInFlight.
		o.StateRef.set(StepStepFinished, step)

		for _, fn := range o.OnStepFinish {
			callHook(o.OnPanic, "OnStepFinish", func() { fn(stepResult) })
		}

		// If no tools have Execute functions, skip the tool loop regardless of MaxSteps.
		// This allows callers to provide tool definitions for the model's awareness
		// without requiring executable tools.
		// No empty-step guard needed (unlike streaming): DoGenerate returns content or error.
		// Normalize: providers that send empty/wrong finish_reason with tool calls
		// (MiniMax, Azure MaaS deepseek, etc.) would cause the loop to exit early.
		// The presence of tool calls is authoritative.
		if len(result.ToolCalls) > 0 && result.FinishReason != provider.FinishToolCalls {
			result.FinishReason = provider.FinishToolCalls
		}

		if result.FinishReason != provider.FinishToolCalls || len(result.ToolCalls) == 0 || len(toolMap) == 0 {
			tr := buildTextResult(steps, totalUsage)
			tr.ResponseMessages = buildResponseMessages(params.Messages[originalLen:], steps, nil)
			// Distinguish "model stopped on its own" from "model wants more
			// tool calls but no tool has Execute" (FIX 11). Both exit cleanly
			// but mean very different things to consumers.
			cause := provider.StopCauseNatural
			if len(result.ToolCalls) > 0 && len(toolMap) == 0 {
				cause = provider.StopCauseNoExecutableTools
			}
			fireOnFinish(o.OnPanic, o.OnFinish, FinishInfo{
				TotalSteps:   len(steps),
				TotalUsage:   totalUsage,
				FinishReason: tr.FinishReason,
				StoppedBy:    cause,
			})
			return tr, nil
		}

		// Execute tools and build continuation messages.
		// Clear tool_choice after the first tool step so the model can freely
		// produce a text response on subsequent steps.
		params.ToolChoice = ""
		o.StateRef.set(StepToolExecuting, step)
		toolMessages, toolResults := executeToolsParallel(ctx, result.ToolCalls, toolMap, step, toolHooks{
			sequential:      o.SequentialTools,
			onToolCallStart: o.OnToolCallStart,
			onToolCall:      o.OnToolCall,
			onBeforeExecute: o.OnBeforeToolExecute,
			onAfterExecute:  o.OnAfterToolExecute,
			onPanic:         o.OnPanic,
		})
		// Attach ToolResults to the step BEFORE the stop predicate sees it
		// (FIX 7 / Vercel DefaultStepResult parity). steps[-1] is this step.
		steps[len(steps)-1].ToolResults = toolResults

		// Append assistant message with tool calls + tool result messages.
		params.Messages = appendToolRoundTrip(params.Messages, result.Text, nil, result.ToolCalls, toolMessages)

		// WithStopWhen (Vercel parity): evaluated AFTER this step's LLM call
		// AND its tool executions complete. The tool-result messages are
		// already folded into params.Messages, so ResponseMessages produced
		// when the loop breaks here is a valid replay transcript (assistant
		// tool_use paired with matching tool_result). Matches
		// vercel-ai/packages/ai/src/generate-text/generate-text.ts where
		// stopWhen() gates the next iteration only after tools have run.
		if o.StopWhen != nil && stopSafe(o.OnPanic, o.StopWhen, steps) {
			hookStopped = true
			stopCause = provider.StopCausePredicate
			break
		}
	}

	// Post-loop: reachable when MaxSteps was exhausted OR when OnBeforeStep.Stop=true
	// caused a break. Only set StepsExhausted when MaxSteps was actually reached AND
	// the last step still had tool calls pending (model wanted to continue but was cut
	// short). This matches StreamText's conditional logic and correctly distinguishes
	// "hook stopped" (StepsExhausted=false) from "max steps" (StepsExhausted=true).
	tr := buildTextResult(steps, totalUsage)
	// In the sync path every early-exit cause (Natural / NoExecutableTools /
	// Abort) returns immediately inside the loop, so the only way we reach
	// here with stopCause != "" is via a hookStopped break (BeforeStep /
	// Predicate). The hookStopped guard alone is therefore sufficient; the
	// former `stopCause == ""` check was a redundant belt-and-suspenders
	// that is intentionally dropped here for clarity. Streaming keeps the
	// extra guard because its loop has a Natural-case break that sets
	// stopCause without setting hookStopped - see streamWithToolLoop.
	var stepsExhausted bool
	stopCause, stepsExhausted = finalizeStopCause(hookStopped, stopCause, steps, o.MaxSteps)
	tr.StepsExhausted = stepsExhausted
	tr.ResponseMessages = buildResponseMessages(params.Messages[originalLen:], steps, nil)
	fireOnFinish(o.OnPanic, o.OnFinish, FinishInfo{
		StepsExhausted: tr.StepsExhausted,
		TotalSteps:     len(steps),
		TotalUsage:     totalUsage,
		FinishReason:   tr.FinishReason,
		StoppedBy:      stopCause,
	})
	return tr, nil
}

// requestMessages returns msgs with a system message prepended when system is non-empty.
// Always allocates a new slice so hooks cannot mutate the caller's message state.
func requestMessages(system string, msgs []provider.Message) []provider.Message {
	if system == "" {
		out := make([]provider.Message, len(msgs))
		copy(out, msgs)
		return out
	}
	out := make([]provider.Message, 0, len(msgs)+1)
	out = append(out, SystemMessage(system))
	return append(out, msgs...)
}

// addUsage adds b's counts to a and returns the result.
func addUsage(a, b provider.Usage) provider.Usage {
	return provider.Usage{
		InputTokens:      a.InputTokens + b.InputTokens,
		OutputTokens:     a.OutputTokens + b.OutputTokens,
		TotalTokens:      a.TotalTokens + b.TotalTokens,
		ReasoningTokens:  a.ReasoningTokens + b.ReasoningTokens,
		CacheReadTokens:  a.CacheReadTokens + b.CacheReadTokens,
		CacheWriteTokens: a.CacheWriteTokens + b.CacheWriteTokens,
	}
}
