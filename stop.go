package goai

import (
	"slices"

	"github.com/zendev-sh/goai/provider"
)

// fireOnFinish calls all OnFinish hooks with individual panic recovery.
// stopSafe evaluates a StopCondition with recover. A panicking predicate is
// treated as "do not stop" and logged to stderr (consistent with how
// OnBeforeStep / OnStepFinish handle panics).
// finalizeStopCause classifies the terminal StopCause when the tool loop
// exits by natural termination (no break set a cause). It encapsulates the
// post-loop MaxSteps-exhaustion guard and the StopCauseNatural default so
// both GenerateText and streamWithToolLoop share one implementation.
//
// Returns the resolved StopCause and whether MaxSteps was exhausted.
func finalizeStopCause(hookStopped bool, current provider.StopCause, steps []StepResult, maxSteps int) (provider.StopCause, bool) {
	stepsExhausted := false
	if !hookStopped && current == "" && len(steps) >= maxSteps && len(steps) > 0 && len(steps[len(steps)-1].ToolCalls) > 0 {
		stepsExhausted = true
		current = provider.StopCauseMaxSteps
	}
	if current == "" {
		current = provider.StopCauseNatural
	}
	return current, stepsExhausted
}

func stopSafe(onPanic []func(PanicInfo), pred StopCondition, steps []StepResult) bool {
	var stop bool
	// Pass a shallow defensive copy so predicates cannot re-order or truncate
	// the internal slice. NOTE: this is a TOP-LEVEL copy only - nested slices
	// (StepResult.ToolCalls, StepResult.ToolResults, StepResult.Content) are
	// ALIASED into the caller's view. Predicates MUST treat the StepResult
	// contents as read-only; mutating nested slices corrupts goai internal
	// state and is a contract violation. Deep-clone would be prohibitively
	// expensive per-step for a feature (predicate side-effects) that is not
	// a supported use case.
	//
	// A panic in the predicate is fired to OnPanic and surfaced as a
	// *PanicError (returned by GenerateText, or via stream.Err() for StreamText).
	callHook(onPanic, "StopWhen", func() { stop = pred(slices.Clone(steps)) })
	return stop
}

func fireOnFinish(onPanic []func(PanicInfo), hooks []func(FinishInfo), info FinishInfo) {
	for _, fn := range hooks {
		callHook(onPanic, "OnFinish", func() { fn(info) })
	}
}
