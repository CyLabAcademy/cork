#!/usr/bin/env bash
# Every example challenge builds, is delivered the way its type says, and is
# solved by its own solver against the flag cork recorded for the build.
#
# usage: ci/examples/run.sh <directory holding corkd and cork>
#
# Starts a corkd of its own on the local docker daemon (no registry, no
# workers) unless CORK_SERVER names one already running, in which case
# CORK_DIR and CORK_ARTIFACT_DIR must be that daemon's. CORK_EXAMPLES limits
# the run to the challenge ids it lists. Needs docker, curl, jq and tar.
set -uo pipefail

if [[ $# -ne 1 || ! -x "$1/corkd" || ! -x "$1/cork" ]]; then
	echo "usage: $0 <directory holding corkd and cork>" >&2
	exit 2
fi
BIN=$(cd "$1" && pwd)
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
WORK=$(mktemp -d)
SEED=1
FORMAT='examples{%s}'
SOLVE_TIMEOUT=600 # what cmgr's solver framework allowed

T0=$(date +%s)
since() { echo $(($(date +%s) - $1)); }
api() { # api <method> <path> [body]: the body on 2xx, else fails with it
	curl -sS --fail-with-body --max-time 1800 -X "$1" -H 'Content-Type: application/json' \
		${3:+-d "$3"} "$CORK_SERVER$2"
}
code() { # code <method> <path> [body]: the status alone
	curl -sS -o /dev/null -w '%{http_code}' --max-time 1800 -X "$1" \
		-H 'Content-Type: application/json' ${3:+-d "$3"} "$CORK_SERVER$2"
}

BUILDS=()
INSTANCES=()
SOLVES=0
CORKD_PID=""
cleanup() {
	local id
	for id in "${INSTANCES[@]}"; do code DELETE "/instances/$id" >/dev/null; done
	for id in "${BUILDS[@]}"; do code DELETE "/builds/$id" >/dev/null; done
	docker ps -aq --filter 'name=^cork-examples-solver-' | xargs -r docker rm -f >/dev/null
	docker images -q cork-examples-solver | sort -u | xargs -r docker rmi -f >/dev/null 2>&1
	if [[ -n "$CORKD_PID" ]]; then
		kill "$CORKD_PID" 2>/dev/null
		wait "$CORKD_PID" 2>/dev/null
	fi
	rm -rf "$WORK"
}
trap cleanup EXIT

if [[ -z "${CORK_SERVER:-}" ]]; then
	unset CORK_REGISTRY CMGR_REGISTRY CORK_BUILD_PLANE CMGR_BUILD_PLANE
	export CORK_DIR=${CORK_DIR:-$REPO/examples}
	export CORK_DB=$WORK/cork.db CORK_ARTIFACT_DIR=$WORK/artifacts
	export CORK_SERVER=http://127.0.0.1:${CORK_EXAMPLES_PORT:-4290}
	"$BIN/corkd" --address 127.0.0.1 --port "${CORK_SERVER##*:}" >"$WORK/corkd.log" 2>&1 &
	CORKD_PID=$!
	for _ in $(seq 60); do
		curl -sSf --max-time 2 -o /dev/null "$CORK_SERVER/version" 2>/dev/null && break
		kill -0 "$CORKD_PID" 2>/dev/null || break
		sleep 1
	done
	if ! curl -sSf --max-time 2 -o /dev/null "$CORK_SERVER/version" 2>/dev/null; then
		echo "corkd did not come up on $CORK_SERVER:" >&2
		tail -30 "$WORK/corkd.log" >&2
		exit 2
	fi
fi
: "${CORK_DIR:?CORK_DIR must name the challenge tree the daemon serves}"
: "${CORK_ARTIFACT_DIR:?CORK_ARTIFACT_DIR must name the artifact directory of that daemon}"

"$BIN/cork" update >"$WORK/update.log" 2>&1 || {
	cat "$WORK/update.log"
	exit 2
}
if [[ -n "${CORK_EXAMPLES:-}" ]]; then
	read -ra CHALLENGES <<<"$CORK_EXAMPLES"
else
	mapfile -t CHALLENGES < <(api GET /challenges | jq -r '.[].id' | sort)
fi
[[ ${#CHALLENGES[@]} -gt 0 ]] || { echo "no challenges under $CORK_DIR" >&2; exit 2; }

# solve <challenge dir> <build json> <network, empty for none>: runs the
# challenge's solver the way examples/solvers.md promises and sets REASON
# when it does not write the build's flag.
solve() {
	local dir=$1 bmeta=$2 net=$3 bid flag ctx img name got
	bid=$(jq -r .id <<<"$bmeta")
	flag=$(jq -r .flag <<<"$bmeta")
	# Numbered per solve, not per build: cork hands a destroyed build's id to
	# the next build.
	SOLVES=$((SOLVES + 1))
	ctx=$WORK/solve-$SOLVES
	img=cork-examples-solver:$SOLVES
	name=cork-examples-solver-$SOLVES
	mkdir -p "$ctx"
	cp -a "$dir/solver/." "$ctx/"
	[[ -f "$ctx/Dockerfile" ]] || cp "$HERE/solver.Dockerfile" "$ctx/Dockerfile"
	if [[ "$(jq -r .has_artifacts <<<"$bmeta")" == true ]]; then
		tar xzf "$CORK_ARTIFACT_DIR/$bid.tar.gz" -C "$ctx" || {
			REASON="could not extract the artifacts of build $bid"
			return 1
		}
	fi
	if [[ "$(jq -r '.lookup_data // {} | length' <<<"$bmeta")" != 0 ]]; then
		jq -c .lookup_data <<<"$bmeta" >"$ctx/metadata.json"
	fi
	if ! docker build -q -t "$img" "$ctx" >"$WORK/solver-build-$bid.log" 2>&1; then
		REASON="the solver image did not build: $(tail -5 "$WORK/solver-build-$bid.log")"
		return 1
	fi
	docker create --name "$name" --hostname solve -t \
		--security-opt "seccomp=$REPO/cork/seccomp.json" \
		${net:+--network "$net" --network-alias solver} "$img" >/dev/null &&
		docker start "$name" >/dev/null || {
		REASON="the solver container did not start"
		return 1
	}
	if ! timeout "$SOLVE_TIMEOUT" docker wait "$name" >/dev/null; then
		REASON="the solver did not exit within ${SOLVE_TIMEOUT}s; its last output: $(docker logs --tail 10 "$name" 2>&1)"
		return 1
	fi
	got=$(docker cp "$name:/solve/flag" - 2>/dev/null | tar xO 2>/dev/null)
	got=${got#"${got%%[![:space:]]*}"}
	got=${got%"${got##*[![:space:]]}"}
	if [[ "$got" != "$flag" ]]; then
		REASON="the solver wrote '${got:-nothing}' and build $bid carries '$flag'; its last output: $(docker logs --tail 10 "$name" 2>&1)"
		return 1
	fi
	docker rm -f "$name" >/dev/null
	docker rmi -f "$img" >/dev/null 2>&1
	return 0
}

# check <challenge id>: builds, delivers, solves and destroys one challenge,
# and sets REASON when any of it goes wrong.
check() {
	local id=$1 cmeta type dir builds bid bmeta flag inst iid cid
	cmeta=$(api GET "/challenges/$id") || { REASON="cork does not know it: $cmeta"; return 1; }
	type=$(jq -r .delivery_type <<<"$cmeta")
	dir=$(dirname "$(jq -r .path <<<"$cmeta")")

	# The disks example builds FROM a base image no registry holds; CI builds
	# it with a layer cache first, and a local run gets it built here.
	if grep -qs '^FROM cmgr/examples-guestfish-base' "$dir/Dockerfile" "$dir/solver/Dockerfile" &&
		! docker image inspect cmgr/examples-guestfish-base >/dev/null 2>&1; then
		echo "   building cmgr/examples-guestfish-base first (slow; CI caches it)"
		docker build -q -t cmgr/examples-guestfish-base -f "$REPO/ci/guestfish-base.Dockerfile" "$REPO/ci" >"$WORK/base.log" 2>&1 || {
			REASON="its base image did not build: $(tail -5 "$WORK/base.log")"
			return 1
		}
	fi

	builds=$(api POST "/challenges/$id" "{\"flag_format\":\"$FORMAT\",\"seeds\":[$SEED]}") || {
		REASON="it did not build: $(tail -c 600 <<<"$builds")"
		return 1
	}
	bid=$(jq -r '.[0].id' <<<"$builds")
	BUILDS+=("$bid")
	bmeta=$(api GET "/builds/$bid") || { REASON="build $bid cannot be read back: $bmeta"; return 1; }
	# A flag-only challenge brings its own flag, so the format proves nothing;
	# the solver is what checks the flag.
	flag=$(jq -r '.flag // ""' <<<"$bmeta")
	[[ -n "$flag" ]] || { REASON="build $bid carries no flag"; return 1; }
	if [[ "$(jq -r .has_artifacts <<<"$bmeta")" == true ]] && ! tar tzf "$CORK_ARTIFACT_DIR/$bid.tar.gz" >/dev/null 2>&1; then
		REASON="build $bid publishes artifacts, but $CORK_ARTIFACT_DIR/$bid.tar.gz is not a readable archive"
		return 1
	fi

	case "$type" in
	service)
		# As the platform launches one: a user and runtime environment.
		inst=$(api POST "/builds/$bid" '{"user_id":"examples","env":{"CUSTOM_VAR":"examples"}}') || {
			REASON="build $bid did not launch: $inst"
			return 1
		}
		iid=$(jq -r .id <<<"$inst")
		INSTANCES+=("$iid")
		[[ "$(jq -r '.ports // {} | length' <<<"$inst")" != 0 ]] || {
			REASON="instance $iid of a service challenge publishes no port: $inst"
			return 1
		}
		if [[ "$(jq -r .solve_script <<<"$cmeta")" != true ]]; then
			REASON="it has no solver, and only a flag-only challenge may go without one"
			return 1
		fi
		solve "$dir" "$bmeta" "cmgr-$iid" || return 1
		[[ "$(code DELETE "/instances/$iid")" == 204 ]] || { REASON="instance $iid did not stop"; return 1; }
		for cid in $(jq -r '.containers[]?' <<<"$inst"); do
			! docker container inspect "$cid" >/dev/null 2>&1 || {
				REASON="container $cid of instance $iid is still there after the stop"
				return 1
			}
		done
		! docker network inspect "cmgr-$iid" >/dev/null 2>&1 || {
			REASON="network cmgr-$iid is still there after the stop"
			return 1
		}
		;;
	artifact_only | flag_only)
		[[ "$(code POST "/builds/$bid" '{}')" != 201 ]] || {
			REASON="a launch of a $type build was answered with an instance"
			return 1
		}
		if [[ "$(jq -r .solve_script <<<"$cmeta")" == true ]]; then
			solve "$dir" "$bmeta" "" || return 1
		elif [[ "$type" != flag_only ]]; then
			REASON="it has no solver, and only a flag-only challenge may go without one"
			return 1
		fi
		;;
	*)
		REASON="cork reports an unknown delivery type '$type'"
		return 1
		;;
	esac

	[[ "$(code DELETE "/builds/$bid")" == 204 ]] || { REASON="build $bid was not destroyed"; return 1; }
	return 0
}

echo "== ${#CHALLENGES[@]} challenges from $CORK_DIR, against $CORK_SERVER on docker $(docker version --format '{{.Server.Version}}') ($(docker info --format '{{.Driver}}'))"
failed=()
for id in "${CHALLENGES[@]}"; do
	t=$(date +%s)
	REASON=""
	if check "$id"; then
		echo "PASS $id ($(since "$t")s)"
	else
		echo "FAIL $id ($(since "$t")s): $REASON"
		failed+=("$id")
	fi
done

echo "== ${#CHALLENGES[@]} challenges, ${#failed[@]} failed, in $(since "$T0")s"
if [[ ${#failed[@]} -ne 0 ]]; then
	[[ -f "$WORK/corkd.log" ]] && { echo "-- last lines of corkd's log"; tail -40 "$WORK/corkd.log"; }
	exit 1
fi
