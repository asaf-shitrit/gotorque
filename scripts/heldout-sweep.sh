#!/usr/bin/env bash
# Held-out stability sweep: the steps in docs/heldout-sweep.md as one script.
#
#   scripts/heldout-sweep.sh build REV     build gotorque at a committed revision
#   scripts/heldout-sweep.sh campaigns     one bounded live campaign per held-out target (SPENDS MONEY)
#   scripts/heldout-sweep.sh verify        re-measure every accepted patch
#   scripts/heldout-sweep.sh null          null candidates per target (no model calls)
#   scripts/heldout-sweep.sh scorecard     sum the verdicts
#
# Every step reads SWEEP_DIR (default ./heldout-sweep) and is safe to re-run: a
# campaign directory that exists is skipped, so an interrupted sweep continues.
# Only `campaigns` calls a model. It refuses to start unless SWEEP_APPROVED=yes.
set -euo pipefail

SWEEP_DIR=${SWEEP_DIR:-$PWD/heldout-sweep}
MAX_LOAD=${MAX_LOAD:-3}                       # 1-minute load average a measurement needs
QUIET_TIMEOUT_MIN=${QUIET_TIMEOUT_MIN:-60}    # wait this long for the machine, then run anyway and say so
MAX_DURATION=${MAX_DURATION:-40m}             # per campaign (the manifests say 90m)
MAX_CANDIDATES=${MAX_CANDIDATES:-6}           # per campaign (the manifests say 12)
STOP_AFTER_FAILURES=${STOP_AFTER_FAILURES:-3} # consecutive failures that end a campaign (manifests: 4)
NULLS_PER_TARGET=${NULLS_PER_TARGET:-7}
VERIFY_PAIRS=${VERIFY_PAIRS:-60}

root=$(cd "$(dirname "$0")/.." && pwd)
bin=$SWEEP_DIR/bin/gotorque
log() { printf '=== %s %s\n' "$(date '+%F %H:%M')" "$*"; }
die() { echo "heldout-sweep: $*" >&2; exit 1; }

# targets reads the held-out set from the table in targets/HELDOUT.md, so the
# list lives in one place.
targets() { sed -n 's/^| `\([a-z0-9-]*\)` .*/\1/p' "$root/targets/HELDOUT.md"; }

load1() {
	if [ -r /proc/loadavg ]; then cut -d' ' -f1 /proc/loadavg; else sysctl -n vm.loadavg | awk '{print $2}'; fi
}

# wait_quiet blocks until the machine is quiet enough to time things on. A
# measurement under foreign load can accept or reject for the wrong reason, so
# the log says which steps ran loaded.
wait_quiet() {
	local waited=0 limit=$((QUIET_TIMEOUT_MIN * 60))
	while :; do
		local load
		load=$(load1)
		if awk -v l="$load" -v m="$MAX_LOAD" 'BEGIN{exit !(l < m)}'; then log "quiet: load $load"; return 0; fi
		if [ "$waited" -ge "$limit" ]; then log "NOT QUIET: load $load after ${QUIET_TIMEOUT_MIN}m, running anyway"; return 1; fi
		sleep 30; waited=$((waited + 30))
	done
}

need_binary() { [ -x "$bin" ] || die "no binary at $bin: run '$0 build REV' first"; }

# repo_for clones a target's repository once and prints its path. The checked-out
# commit is appended to revisions.tsv, because the manifests name a repository,
# not a commit.
repo_for() {
	local target=$1 dir=$SWEEP_DIR/repos/$1
	if [ ! -d "$dir/.git" ]; then
		git clone --quiet "$(jq -r .target.repository "$root/targets/$target/manifest.json")" "$dir"
		printf '%s\t%s\n' "$target" "$(git -C "$dir" rev-parse HEAD)" >>"$SWEEP_DIR/revisions.tsv"
	fi
	echo "$dir"
}

cmd_build() {
	local rev=${1:-}
	[ -n "$rev" ] || die "usage: $0 build REV"
	rev=$(git -C "$root" rev-parse --verify "$rev^{commit}")
	mkdir -p "$SWEEP_DIR/bin"
	rm -rf "$SWEEP_DIR/src"
	git -C "$root" worktree add --detach --quiet "$SWEEP_DIR/src" "$rev"
	# The commit goes in through ldflags, as a release build does, so `version`
	# and the build recorded in every campaign name the revision under test.
	(cd "$SWEEP_DIR/src" && go build -ldflags "-X github.com/asaf-shitrit/gotorque/internal/version.Commit=$rev" -o "$bin" ./cmd/gotorque)
	git -C "$root" worktree remove --force "$SWEEP_DIR/src"
	"$bin" version | tee "$SWEEP_DIR/version.txt"
	log "built $rev"
}

cmd_campaigns() {
	need_binary
	[ "${SWEEP_APPROVED:-}" = yes ] || die "this step calls the optimizer model and Jev and spends credit; set SWEEP_APPROVED=yes once the budget is approved"
	[ -n "${OPENROUTER_API_KEY:-}" ] || die "OPENROUTER_API_KEY is not set"
	{
		echo "optimizer model: ${GOTORQUE_MODEL_OPTIMIZER:-default}"
		echo "endpoint: ${OPENROUTER_BASE_URL:-https://openrouter.ai/api/v1}"
		echo "bounds: max_duration=$MAX_DURATION max_candidate_patches=$MAX_CANDIDATES stop_after_failures=$STOP_AFTER_FAILURES"
	} >"$SWEEP_DIR/settings.txt"
	mkdir -p "$SWEEP_DIR/campaigns" "$SWEEP_DIR/manifests"
	local t
	for t in $(targets); do
		local dir=$SWEEP_DIR/campaigns/$t
		[ -d "$dir" ] && { log "$t: campaign exists, skipped"; continue; }
		# The checked-in manifest with only the campaign bounds tightened.
		jq --arg d "$MAX_DURATION" --argjson n "$MAX_CANDIDATES" --argjson f "$STOP_AFTER_FAILURES" \
			'.campaign.max_duration=$d | .campaign.max_candidate_patches=$n | .campaign.stop_after_failures=$f' \
			"$root/targets/$t/manifest.json" >"$SWEEP_DIR/manifests/$t.json"
		"$bin" manifest validate "$SWEEP_DIR/manifests/$t.json" >/dev/null
		wait_quiet || true
		log "$t: start"
		"$bin" optimize --repo "$(repo_for "$t")" --manifest "$SWEEP_DIR/manifests/$t.json" --campaign-dir "$dir" --adk >"$dir.log" 2>&1 || log "$t: exit $?"
		rm -rf "$dir/builds"
		# Out of credit: every later campaign would fail the same way.
		if grep -q 'HTTP 402' "$dir.log"; then log "sweep aborted after $t: HTTP 402 (out of credit)"; return 1; fi
	done
	log "campaigns done"
}

cmd_verify() {
	need_binary
	local dir
	for dir in "$SWEEP_DIR"/campaigns/*/; do
		[ -d "$dir" ] || continue
		wait_quiet || true
		log "verify $(basename "$dir")"
		# Exits non-zero when the campaign accepted nothing. A verification that
		# does not hold prints "held: false" and still exits 0; either way the
		# verdicts are saved in the campaign state, which the scorecard reads.
		"$bin" verify "${dir%/}" --pairs "$VERIFY_PAIRS" >"${dir%/}.verify.log" 2>&1 || log "verify $(basename "$dir"): exit $? (see ${dir%/}.verify.log)"
	done
}

cmd_null() {
	need_binary
	mkdir -p "$SWEEP_DIR/null"
	local t
	for t in $(targets); do
		local dir=$SWEEP_DIR/null/$t
		[ -d "$dir" ] && { log "$t: null campaign exists, skipped"; continue; }
		wait_quiet || true
		log "$t: $NULLS_PER_TARGET null candidates"
		"$bin" optimize --repo "$(repo_for "$t")" --manifest "$root/targets/$t/manifest.json" --campaign-dir "$dir" --null-candidates "$NULLS_PER_TARGET" >"$dir.log" 2>&1 || log "$t: exit $?"
		rm -rf "$dir/builds"
	done
}

cmd_scorecard() {
	need_binary
	local dirs=() d
	for d in "$SWEEP_DIR"/campaigns/*/ "$SWEEP_DIR"/null/*/; do
		[ -d "$d" ] && dirs+=("${d%/}")
	done
	[ ${#dirs[@]} -gt 0 ] || die "no campaign directories under $SWEEP_DIR"
	"$bin" scorecard "${dirs[@]}" | tee "$SWEEP_DIR/scorecard.md"
}

mkdir -p "$SWEEP_DIR"
step=${1:-}
[ $# -gt 0 ] && shift
case $step in
build | campaigns | verify | null | scorecard) "cmd_$step" "$@" ;;
*) die "usage: $0 build REV | campaigns | verify | null | scorecard" ;;
esac
