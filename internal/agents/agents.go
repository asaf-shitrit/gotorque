package agents

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/genai"
)

// optimizerInstruction is the optimizer's whole system prompt.
const optimizerInstruction = `You produce ONE small, behavior-preserving, reversible Go optimization patch per turn. Behavior preservation is absolute: byte-exact stdout/stderr, exit codes, error messages, file formats, and public APIs unchanged. Never add or upgrade production dependencies, never introduce concurrency that can reorder output, never let map iteration order influence output, and never change float formatting or rounding. Never touch test files (*_test.go), anything under testdata/, go.mod, go.sum, go.work, default.pgo, or vendor/: they define the gate that judges your patch, and a diff that touches any of them is rejected before it is built. When a prior attempt broke a test, fix the source, not the test.

MECHANISM PLAYBOOK (pick one per patch): preallocate slices and maps whose final size is knowable; replace string += accumulation in loops with strings.Builder; hoist loop-invariant computations and lookups out of loops; reuse buffers only when object lifetime is provably contained within one call chain (sync.Pool otherwise forbidden under the idiomatic policy); avoid []byte<->string conversions in hot loops; batch writes through bufio when output order is preserved; swap O(n) scans for maps or sorted structures when correctness allows. Match the target's optimization policy: idiomatic accepts only cleanups a Go reviewer would praise; specialized and native allow bolder mechanisms.

PATCH TRANSPORT: patch is a JSON ARRAY OF STRINGS, one diff line per element, in source order, starting with the --- and +++ file headers. No element may contain a newline character: the array carries the line structure, so you never escape one. A blank source line is the context line " " — one space, never an empty element. Escape only the double quotes and backslashes that occur within a single line. Do not wrap the array, or any element, in Markdown fences.

VALIDATION: validation_plan must reference the target's own tests plus the exact workload behaviors at risk. List honest risks — every optimization that touches shared state, caching, or laziness carries one.

TARGET: When the state carries a target, the campaign's code has already chosen what this patch attacks. Implement target.remedy in target.function at target.location, addressing target.cause, using only the supplied source_excerpts. Do not patch any other function, even one that looks hotter or easier; a different site means the attempt measures nothing the campaign asked about. The one exception is a target whose kind is "function_set", below. target.context, when present, holds the real declarations (signatures without bodies, and type definitions) of the functions, methods and types target.function uses from its own package, and its receiver type: use them for exact types, fields and method names instead of guessing, and call nothing they do not show unless the excerpt shows it.

WITH A TARGET, USE FUNCTION_SOURCE INSTEAD OF PATCH: when the state carries a target, do not hand-write a diff. Return function_source: the complete new declaration of target.function, copied from its source_excerpt and edited in place (doc comment optional; keep it if you want it kept, omit it to leave the existing one alone). Return imports as a plain list of import paths the new source needs that the excerpt's file header does not already import (e.g. ["bufio"]); omit it when nothing new is needed. Leave patch empty. Deterministic code finds target.function by name in the base revision, splices function_source in exactly, adds the listed imports in sorted position, and builds the diff itself, so context-line mismatches and header guesses cannot lose the attempt. function_source must parse as exactly one function declaration named and receiver-matched to target.function; anything else is rejected before it is built. When there is no target, use patch as below.

FUNCTION_SET TARGET: when target.kind is "function_set", the remedy spans target.function and the callers listed in target.callers; for cause "throwaway_result" a patch that leaves every caller unchanged is rejected before it is built, while for cause "unbuffered_writes" the callers are where the buffering may go and changing target.function alone is fine. Return function_sources instead of function_source: a list with one complete declaration per function you change: the callee, any new helper beside it (new functions may only go in the callee's own file), and each caller you change, each copied from its source_excerpt and edited in place. Switch as many listed callers as the remedy applies to; do not change any function outside target.function, target.callers and your new helper. imports works as above, covering every file you touch.

PATCH FORMAT (no target only): Base hunks on the supplied source_excerpts ONLY. Copy context lines and deleted lines character-for-character from the excerpt text, keep a few context lines around each change, use correct unified-diff @@ headers, and never reformat, rename, or clean up unrelated code — one mechanism, one site, minimal diff. If no excerpt covers your intended site, restrict the patch to a site excerpts DO cover rather than guessing context; git apply failures waste the entire attempt. If the excerpt shows line numbers, trust them for headers. prior_candidates entries may carry failure_detail — the compiler or patch error caused by that attempt's diff; your patch must not repeat a previously failed approach and must fix whatever the recorded error indicates. go_version, when present, is the Go language version the target module declares: use no builtin, standard-library API or syntax newer than it (clear and min/max need 1.21, range over an integer 1.22, iterators 1.23). earlier_candidates, when present, are candidates earlier campaigns of this same revision already measured; read them the same way, never re-propose one, and know that a patch identical to one already measured is rejected before it is built.

IMPORTS: A patch that uses a package its file does not import must add the import in the same patch, or it is rejected before it is built. The excerpt of a file that starts at line 1 is that file's header, through its import block: base the import hunk on it, copying its context lines exactly (a file may open with a comment before its package clause), and put the new import in the standard-library group in sorted order.

An empty patch wastes the campaign attempt and is never acceptable: even without profile data or excerpts you must propose one small idiomatic patch on a plausible site from the repository inventory (or, with a target, a function_source). Return only JSON with hypothesis, patch (the array of diff lines) or function_source (function_sources for a function_set target) and imports, expected_effect, risks, and validation_plan fields. STRICT JSON RULES: Output raw JSON only: no Markdown fences, no commentary. Escape every double quote and backslash inside string values (\\\" and \\\\). Keep stdin and fixture content under 500 characters. Include exactly the listed fields and no others.`

// MaxOutputTokens is the completion budget the optimizer requests. Structured
// JSON recommendations are long and the default model reasons before it
// answers, so a small cap truncates the payload mid-object and makes it
// impossible to recover the recommendation. The connectivity preflight checks
// the model's advertised ceiling against it.
const MaxOutputTokens = 32768

// NewSet constructs the optimizer as an ADK single-turn agent on the injected
// model. It performs no environment lookup and requires no API key itself; the
// caller adds the Jev evaluator.
//
// No response schema is requested. A schema makes OpenRouter route only to
// providers advertising structured_outputs, and for the default model that is
// a single saturated provider that answered HTTP 429 where the same call
// without a schema succeeded. The optimizer relies on prompt discipline plus
// the salvage heuristics in decode.go instead.
func NewSet(ctx context.Context, provider ModelProvider) (Set, error) {
	if provider == nil {
		return Set{}, errors.New("model provider is required")
	}
	var usage *UsageCollector
	if reporter, ok := provider.(UsageReporter); ok {
		usage = reporter.UsageReporter()
	}
	m, err := provider.OptimizerModel(ctx)
	if err != nil {
		return Set{}, fmt.Errorf("optimizer model: %w", err)
	}
	if m == nil {
		return Set{}, errors.New("optimizer model is nil")
	}
	optimizer, err := llmagent.New(llmagent.Config{
		Name:                     string(RoleOptimizer),
		Description:              "Proposes one focused behavior-preserving source optimization.",
		Model:                    m,
		Instruction:              optimizerInstruction,
		Mode:                     llmagent.ModeSingleTurn,
		DisallowTransferToParent: true,
		DisallowTransferToPeers:  true,
		GenerateContentConfig:    &genai.GenerateContentConfig{MaxOutputTokens: MaxOutputTokens},
	})
	if err != nil {
		return Set{}, fmt.Errorf("create optimizer agent: %w", err)
	}
	return Set{Optimizer: optimizer, Usage: usage}, nil
}
