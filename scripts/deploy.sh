#!/usr/bin/env bash
#
# scripts/deploy.sh — install the svpchain research agent onto a remote SSH host
# as a docker container.
#
# This agent is the intermediary: it accepts a task under a re-delegable
# SVP-DT credential, narrows it, and hands the execution leg to a downstream
# agent, which executes on the ORIGINAL user's account. It serves on :8081,
# advertised at <public-url>/research. It is deployed independently: its
# sibling agents (perps, evm, lending) each own their own repo and script, so
# nothing here knows or cares about them.
#
# Flow: build (vendored, so the go.mod replace to ../svpagent/protocol never
# leaves the operator) → docker save (cached by image id) → rsync one staging
# dir (agent.toml, docker-compose.yml, operator.key at 0600) plus the image tar
# to ~/svpchain-research-agent → docker load → docker compose up -d →
# smoke-test /healthz and the agent card over loopback.
#
# AN OPERATOR KEY IS REQUIRED. Unlike its siblings this agent cannot run
# keyless: it SIGNS the credentials it re-delegates, and one signed by nobody
# verifies nowhere. The deploy refuses without --operator-key-file rather than
# installing an agent that cannot answer. The key must also be DISTINCT from
# every other agent's — an agent's on-chain id derives from it and
# agent_self_register publishes a hash of this binary's own card, so a shared
# key makes two agents collide on one registry record. With the agents in
# separate repos nothing here can check that; it is an operational rule.
#
# This binary takes its listen address and advertised URL as FLAGS (-listen,
# -public-url), not from agent.toml — the compose command passes both. The
# rendered listen_addr / public_url exist because core's config schema requires
# the first and documents the second; all of it comes from AGENT_PORT and
# AGENT_SEGMENT below, so the flags and the file cannot disagree.
#
# The remote needs only docker + the compose v2 plugin reachable by the ssh
# user without sudo. Auth state is in-memory, so a redeploy wipes it.
#
# Required:
#   --host user@hostname           SSH target.            SVPCHAIN_DEPLOY_HOST
#   --operator-key-file <path>     LOCAL hex eth_secp256k1 key, shipped 0600
#                                  beside the config. This agent signs the
#                                  credentials it re-delegates, so the deploy
#                                  refuses without one.
#
# Chain endpoints:
#   --chain-id <id>                SVPCHAIN_CHAIN_ID     (svp-2517-1)
#   --grpc-addr <host:port>        SVPCHAIN_GRPC_ADDR    (127.0.0.1:9090)
#   --comet-rpc <url>              SVPCHAIN_COMET_RPC    (http://127.0.0.1:26657)
#   --indexer <url>                SVPCHAIN_INDEXER      (http://127.0.0.1:3002)
#   --agent-chain-id <id>          Optional separate x/agent + x/agentwallet
#   --agent-chain-rest <url>       chain over its Cosmos REST API. Both or
#                                  neither; unset, those families run against
#                                  the DEX chain connection.
#
# Identity:
#   --public-url <url>             Base URL; this agent advertises <base>/research.
#   --operator-capabilities <csv>  Default "research,delegation.redelegate".
#   --operator-metadata <text>
#
# Build and placement:
#   --image-tag <tag>              Default <git-short-sha>.
#   --platform <p>                 Default linux/amd64.
#   --skip-build                   Reuse the local image.
#   --install-dir <path>           Default ~/svpchain-research-agent on remote.
#
# Modes:
#   --print-config / --print-compose / --print-nginx
#   --dry-run / --uninstall
#
# Examples:
#   ./scripts/deploy.sh --host www@svpdev1.example.com
#   ./scripts/deploy.sh --host www@svpdev1.example.com \
#     --operator-key-file ./research.key --public-url https://agents.svpchain.org
#   ./scripts/deploy.sh --uninstall --host www@svpdev1.example.com
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

fail() { printf "  ${C_RED}✗${C_RESET} %s\n" "$*" >&2; exit 1; }

# ---- args ------------------------------------------------------------------

mode="install"        # install | uninstall | print-config | print-compose | print-nginx

host=""
chain_id="${SVPCHAIN_CHAIN_ID:-svp-2517-1}"
grpc_addr="${SVPCHAIN_GRPC_ADDR:-127.0.0.1:9090}"
comet_rpc="${SVPCHAIN_COMET_RPC:-http://127.0.0.1:26657}"
indexer="${SVPCHAIN_INDEXER:-http://127.0.0.1:3002}"
agent_chain_id="${SVPCHAIN_AGENT_CHAIN_ID:-}"
agent_chain_rest="${SVPCHAIN_AGENT_CHAIN_REST:-}"
public_url="${SVPCHAIN_AGENT_PUBLIC_URL:-https://agent-testnet.svpchain.org}"
operator_key_file="${SVPCHAIN_AGENT_OPERATOR_KEY_FILE:-}"
operator_capabilities="research,delegation.redelegate"
operator_metadata=""
install_dir="~/svpchain-research-agent"
image_tag=""
platform="linux/amd64"
skip_build="0"
dry_run="0"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --host)                   host="$2";              shift 2 ;;
    --chain-id)               chain_id="$2";          shift 2 ;;
    --grpc-addr)              grpc_addr="$2";         shift 2 ;;
    --comet-rpc)              comet_rpc="$2";         shift 2 ;;
    --indexer)                indexer="$2";           shift 2 ;;
    --agent-chain-id)         agent_chain_id="$2";    shift 2 ;;
    --agent-chain-rest)       agent_chain_rest="$2";  shift 2 ;;
    --public-url)             public_url="$2";        shift 2 ;;
    --operator-key-file)      operator_key_file="$2"; shift 2 ;;
    --operator-capabilities)  operator_capabilities="$2"; shift 2 ;;
    --operator-metadata)      operator_metadata="$2"; shift 2 ;;
    --install-dir)            install_dir="$2";       shift 2 ;;
    --image-tag)              image_tag="$2";         shift 2 ;;
    --platform)               platform="$2";          shift 2 ;;
    --skip-build)             skip_build="1";         shift ;;
    --print-config)           mode="print-config";    shift ;;
    --print-compose)          mode="print-compose";   shift ;;
    --print-nginx)            mode="print-nginx";     shift ;;
    --dry-run)                dry_run="1";            shift ;;
    --uninstall)              mode="uninstall";       shift ;;
    -h|--help)
      sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed -n '/^#/p' | sed 's/^# \{0,1\}//'
      exit 0 ;;
    *) fail "unknown flag: $1" ;;
  esac
done

: "${host:=${SVPCHAIN_DEPLOY_HOST:-}}"

# Strip a trailing slash (from the flag or env) so the card's
# "<public_url>/invoke" join stays clean.
public_url="${public_url%/}"

# ---- this agent ------------------------------------------------------------
#
# AGENT_PORT and AGENT_SEGMENT are the whole route contract, each stated once.
# The port lands in listen_addr, in the nginx proxy_pass upstream and in the
# smoke test; the segment lands in the advertised public_url and in the nginx
# location. Two copies of either fact is how an agent ends up advertising a URL
# that 404s with every process healthy and nothing in the logs, which is why
# TestDeployScriptNginxRouteMatchesConfig pins the two renderers together by
# cross-checking --print-config against --print-nginx.
readonly AGENT_NAME="svpchain-research-agent"
readonly AGENT_PORT="8081"
readonly AGENT_SEGMENT="research"
readonly IMAGE_REPO="ghcr.io/svpchain/svpchain-research-agent"

# The advertised URL: the base plus this agent's segment — a reverse proxy
# routes that path here. Computed once, after the trailing-slash strip, so the
# config, the nginx block and the preflight banner cannot disagree.
agent_public_url="${public_url}/${AGENT_SEGMENT}"

# Absolute path to the local operator key file; empty means keyless. Set once
# by resolve_operator_key, which every mode runs before rendering anything.
operator_key=""

# ---- shared helpers -------------------------------------------------------

# emit_operator_capabilities — render the capabilities list as a TOML array.
emit_operator_capabilities() {
  local out="[" first=1 cap
  local saved_ifs="$IFS"; IFS=','
  for cap in $operator_capabilities; do
    [[ -z "$cap" ]] && continue
    [[ "$first" == "1" ]] || out+=", "
    out+="\"$cap\""
    first=0
  done
  IFS="$saved_ifs"
  out+="]"
  printf '%s' "$out"
}

# render_agent_toml — emit this agent's agent.toml on stdout. Takes no
# arguments on purpose: --print-config and the deploy render it the same way
# from the same globals, so a preview is the file that ships.
#
# This is a short file because wire.DelegationProfile registers no operation
# families at all: no market data, no funds, no faucet, no EVM, no Lendora, and
# no markets cache. What is left is the chain access the credential verifier
# needs and the operator identity it signs with. Keys for families this binary
# never builds would be read and ignored, so they are not rendered.
#
# listen_addr and public_url are the exception: this binary takes both as flags
# (-listen, -public-url, passed by the compose command), but core's schema
# requires listen_addr, so it is rendered from the same AGENT_PORT the flag
# uses — the file and the flags cannot drift apart.
render_agent_toml() {
  cat <<EOF
# Auto-generated by scripts/deploy.sh — do not edit by hand.
# Agent: ${AGENT_NAME}
#
# NOTE: this binary reads its listen address and advertised URL from the
# -listen / -public-url flags in docker-compose.yml, not from this file.

listen_addr      = "0.0.0.0:${AGENT_PORT}"
public_url       = "${agent_public_url}"

[dex_chain]
id               = "${chain_id}"
grpc_addr        = "${grpc_addr}"
comet_rpc_url    = "${comet_rpc}"
indexer_base_url = "${indexer}"
EOF
  # A separate chain carrying x/agent + x/agentwallet, reached over its
  # Cosmos REST API; unset, the agent-identity families run against the DEX
  # chain connection.
  if [[ -n "$agent_chain_id" || -n "$agent_chain_rest" ]]; then
    [[ -n "$agent_chain_id" && -n "$agent_chain_rest" ]] || \
      fail "--agent-chain-id and --agent-chain-rest must be set together"
    echo ""
    echo "[agent_chain]"
    echo "id       = \"${agent_chain_id}\""
    echo "rest_url = \"${agent_chain_rest}\""
  fi
  # This agent's identity, not an optional extra: it signs every credential it
  # re-delegates, and its agent id derives from this key. key_file is left
  # relative ("operator.key") on purpose — svpchain-agent-core/config resolves
  # it against the agent.toml directory, so it points at the file mounted
  # beside the config. Only --print-config can reach here without a key; the
  # install path refuses earlier.
  if [[ -n "$operator_key" ]]; then
    cat <<EOF

[operator]
key_file     = "operator.key"
capabilities = $(emit_operator_capabilities)
metadata     = "${operator_metadata}"
EOF
  fi
}

# render_compose_yaml — emit the docker-compose.yml that runs this agent: the
# image, its config mount, its data volume and its TCP port. Volumes use
# absolute host paths so `docker compose up -d` works from any directory. The
# rendered config, the operator key and the data volume live flat in
# ${install_dir}; the key is mounted read-only beside the config so the
# config-dir-relative key_file = "operator.key" resolves.
render_compose_yaml() {
  echo "# Auto-generated by scripts/deploy.sh — do not edit by hand."
  echo "services:"
  # ★ ARGS ONLY — no binary path. This image declares
  # ENTRYPOINT ["/bin/svpchain-research-agent"], and compose `command:`
  # overrides CMD, not ENTRYPOINT. Naming the binary here (as the old shared
  # multi-binary image required, since it had no ENTRYPOINT) would launch
  # `svpchain-research-agent svpchain-research-agent -config …` and die on flag
  # parsing.
  cat <<EOF
  ${AGENT_NAME}:
    image: ${image_ref}
    container_name: ${AGENT_NAME}
    restart: unless-stopped
    # -listen and -public-url are flags, not config keys, for this binary: it
    # serves its own intermediary endpoint rather than the shared A2A server.
    # Both come from AGENT_PORT/AGENT_SEGMENT, the same constants that render
    # agent.toml and the nginx block.
    command: ["-config", "/etc/${AGENT_NAME}/agent.toml", "-listen", "0.0.0.0:${AGENT_PORT}", "-public-url", "${agent_public_url}"]
    # network_mode: host — the listener binds 0.0.0.0:${AGENT_PORT} (compose
    # \`ports:\` is ignored in host mode; the port comes from -listen above).
    network_mode: host
    volumes:
      - ${install_dir}/agent.toml:/etc/${AGENT_NAME}/agent.toml:ro
      - ${install_dir}/data:/var/lib/${AGENT_NAME}
EOF
  # An explicit if, not `[[ … ]] && echo`: a false test as the last command
  # would make the function return non-zero, and under `set -e` the
  # `render_compose_yaml > file` call site would exit the script silently.
  if [[ -n "$operator_key" ]]; then
    echo "      - ${install_dir}/operator.key:/etc/${AGENT_NAME}/operator.key:ro"
  fi
}

require_install_args() {
  [[ -n "$host" ]] || fail "--host is required (or set SVPCHAIN_DEPLOY_HOST)"
  # An intermediary signs the credentials it re-delegates, and main.go refuses
  # to start without a key ("an intermediary must sign the credentials it
  # passes on"). Refuse here instead of shipping a container that exits on
  # boot — the older multi-agent script silently skipped this agent and
  # reported a successful deploy that had installed nothing.
  [[ -n "$operator_key_file" ]] || \
    fail "--operator-key-file is required: this agent signs the credentials it re-delegates and cannot run keyless (or set SVPCHAIN_AGENT_OPERATOR_KEY_FILE)"
}

# validate_hex_key — a file must look like a 32-byte hex operator key.
validate_hex_key() {
  grep -Eq '^(0x)?[0-9a-fA-F]{64}[[:space:]]*$' "$1" \
    || fail "operator key '$1' does not look like a 32-byte hex key"
}

# resolve_operator_key — find the operator key from --operator-key-file.
# Without one the agent runs keyless: it advertises execution but refuses with
# a reason. The path resolves against the operator's CWD, so this must run
# before any cd.
#
# The key must be distinct from every other agent's — an agent's on-chain id
# derives from it and agent_self_register hashes this binary's own card, so a
# shared key makes two agents collide on one registry record. With the agents
# in separate repos nothing can check that here; it is an operational rule.
resolve_operator_key() {
  [[ -n "$operator_key_file" ]] || return 0
  local src="$operator_key_file"
  [[ "$src" = /* ]] || src="$(pwd)/$src"
  [[ -f "$src" ]] || fail "--operator-key-file '$src' was not found"
  validate_hex_key "$src"
  operator_key="$src"
}

# resolve_remote_install_dir — expand a leading ~ in $install_dir to the
# remote $HOME (docker bind-mounts need absolute host paths).
resolve_remote_install_dir() {
  case "$install_dir" in
    "~"|"~/"*)
      [[ "$dry_run" == "1" ]] && return 0
      local home
      home="$(ssh -o BatchMode=yes "$host" 'printf %s "$HOME"')" \
        || fail "could not resolve remote \$HOME on $host"
      [[ -n "$home" ]] || fail "remote \$HOME is empty on $host"
      install_dir="${home}${install_dir#\~}"
      ;;
  esac
}

run_or_print() {
  if [[ "$dry_run" == "1" ]]; then
    printf "  [dry-run] %s\n" "$*"
  else
    eval "$@"
  fi
}

remote_exec() {
  run_or_print "ssh -o BatchMode=yes '$host' $(printf '%q ' "$@")"
}

remote_image_id() {
  local img="$1"
  if [[ "$dry_run" == "1" ]]; then
    echo ""
    return
  fi
  ssh -o BatchMode=yes "$host" "docker image inspect --format '{{.Id}}' $img 2>/dev/null || true"
}

local_image_id() {
  docker image inspect --format '{{.Id}}' "$1" 2>/dev/null || true
}

# save_if_changed IMG TAR — docker save IMG to TAR, skipped when TAR.id
# already matches the current image id.
save_if_changed() {
  local img="$1" tar="$2" id
  if [[ "$dry_run" == "1" ]]; then
    info "[dry-run] would docker save $img → $(basename "$tar") (if image id changed)"
    run_or_print "docker save -o '$tar' '$img'"
    return 0
  fi
  id="$(local_image_id "$img")"
  [[ -n "$id" ]] || fail "image $img not found locally; build failed?"
  if [[ -f "$tar" && -f "${tar}.id" && "$(cat "${tar}.id")" == "$id" ]]; then
    info "$img unchanged — skipping save"
    return 0
  fi
  info "$img → $(basename "$tar")"
  run_or_print "docker save -o '$tar' '$img'"
  echo "$id" > "${tar}.id"
}

# load_if_missing IMG REMOTE_TAR EXPECTED_ID — docker load on the remote only
# when the remote doesn't already have IMG at EXPECTED_ID.
load_if_missing() {
  local img="$1" remote_tar="$2" expected_id="$3"
  local remote_id; remote_id="$(remote_image_id "$img")"
  if [[ "$remote_id" == "$expected_id" && -n "$expected_id" ]]; then
    info "$img already loaded on remote — skipping load"
    return 0
  fi
  remote_exec "docker load < $remote_tar"
}

# render_nginx_conf — this agent's location block for the shared reverse proxy.
#
# The path convention is that each agent hangs off one base host at its own
# segment (/perps, /evm, /lending, /research) while listening on its own local
# port. That mapping lives in exactly two constants — AGENT_SEGMENT and
# AGENT_PORT, the same two that build public_url and the listener — so a route
# printed here cannot disagree with what deployed.
#
# Nothing installs this. The server block it belongs in owns TLS and the base
# host, which are outside this repo and shared with agents this repo must not
# know about; four scripts racing to edit one nginx file is how you get a
# half-written config on reload. Print it, review it, paste it.
render_nginx_conf() {
  cat <<EOF
# ${AGENT_NAME} — generated by scripts/deploy.sh --print-nginx
# Paste into the server block for $(printf '%s' "${public_url#*://}"), then
# \`nginx -t && systemctl reload nginx\`.

# Bare /${AGENT_SEGMENT} would 404: the location below only matches the trailing slash.
location = /${AGENT_SEGMENT} { return 301 /${AGENT_SEGMENT}/; }

location /${AGENT_SEGMENT}/ {
    # Trailing slash strips the /${AGENT_SEGMENT} prefix. The agent binds at root and
    # serves /.well-known/agent-card.json and /invoke there; it only knows
    # about /${AGENT_SEGMENT} as the public_url it advertises inside the card.
    proxy_pass http://127.0.0.1:${AGENT_PORT}/;

    proxy_set_header Host              \$host;
    proxy_set_header X-Real-IP         \$remote_addr;
    proxy_set_header X-Forwarded-For   \$proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto \$scheme;

    # A2A streams task updates over SSE. Without HTTP/1.1 and unbuffered
    # proxying nginx holds the events until the response ends, which turns a
    # stream into one delivery at the end and looks like a hung agent.
    proxy_http_version 1.1;
    proxy_set_header Connection "";
    proxy_buffering off;
    proxy_cache off;
    proxy_read_timeout 300s;
}
EOF
}

# ---- mode: print-config ---------------------------------------------------

if [[ "$mode" == "print-config" ]]; then
  # Preview the agent.toml this deploy would ship, [operator] block included
  # when --operator-key-file supplies a key.
  resolve_operator_key
  render_agent_toml
  exit 0
fi

# ---- mode: print-compose --------------------------------------------------

if [[ "$mode" == "print-compose" ]]; then
  # Preview the docker-compose.yml. Uses a placeholder install_dir/image when
  # not resolved, and reflects --operator-key-file so a keyed deploy shows its
  # operator.key mount.
  resolve_operator_key
  image_ref="${IMAGE_REPO}:${image_tag:-<tag>}"
  render_compose_yaml
  exit 0
fi

# ---- mode: print-nginx ----------------------------------------------------

if [[ "$mode" == "print-nginx" ]]; then
  render_nginx_conf
  exit 0
fi

# ---- mode: uninstall ------------------------------------------------------

if [[ "$mode" == "uninstall" ]]; then
  [[ -n "$host" ]] || fail "--host is required (or set SVPCHAIN_DEPLOY_HOST)"
  step "svpchain agents uninstall on $host"
  resolve_remote_install_dir
  remote_exec "docker compose -f $install_dir/docker-compose.yml down 2>/dev/null || true"
  # Belt-and-braces: remove the container by name in case the compose file is
  # gone, then the image, then the install dir.
  remote_exec "docker rm -f $AGENT_NAME 2>/dev/null || true"
  remote_exec "sh -c 'docker images --format \"{{.Repository}}:{{.Tag}}\" $IMAGE_REPO 2>/dev/null | xargs -r docker rmi 2>/dev/null || true'"
  remote_exec "rm -rf $install_dir"
  step "Done"
  exit 0
fi

# ---- mode: install --------------------------------------------------------

require_install_args
require_cmd docker
require_cmd rsync
require_cmd ssh
require_cmd go

# Resolve the operator key path (against the operator's CWD, before any cd) and
# validate it. With no key the agent runs keyless.
resolve_operator_key

REPO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "$REPO_DIR"

if [[ -z "$image_tag" ]]; then
  if image_tag="$(git rev-parse --short HEAD 2>/dev/null)"; then :
  else image_tag="dev"; fi
fi
image_ref="${IMAGE_REPO}:${image_tag}"
image_tar="${REPO_DIR}/build/${AGENT_NAME}.image.tar"
mkdir -p "${REPO_DIR}/build"

step "Preflight (operator + remote)"
info "host=$host image=$image_ref platform=$platform"
info "install_dir=$install_dir public_url=$agent_public_url"
if [[ -n "$operator_key" ]]; then
  info "  ${AGENT_NAME} :${AGENT_PORT} — key ${operator_key} (execution ON)"
else
  info "  ${AGENT_NAME} :${AGENT_PORT} — keyless (execution refuses with a reason)"
fi
if [[ "$dry_run" != "1" ]]; then
  ssh -o BatchMode=yes "$host" "docker version --format '{{.Server.Version}}'" \
    >/dev/null 2>&1 \
    || fail "remote docker not reachable at $host without sudo (ssh keys ok? docker installed? ssh user in the docker group?)"
  ssh -o BatchMode=yes "$host" "docker compose version" >/dev/null 2>&1 \
    || fail "remote 'docker compose' (v2 plugin) not available at $host"
  pass "remote docker + compose reachable"
else
  info "[dry-run] skipping ssh-to-docker reachability check"
fi

resolve_remote_install_dir
info "install_dir=$install_dir"

# Phase 1: build (On operator)
step "On operator: docker build --platform $platform"
if [[ "$skip_build" == "1" ]]; then
  info "--skip-build: reusing existing local image $image_ref"
  [[ -n "$(local_image_id "$image_ref")" ]] || fail "image $image_ref not found locally; drop --skip-build"
else
  # Vendored build (see cmd/svpchain-research-agent/Dockerfile): the go.mod
  # replace to ../svpagent/protocol resolves on the operator, and the vendored
  # tree makes the Docker context self-contained.
  run_or_print "go mod vendor"
  build_cmd="docker build --platform $platform"
  build_cmd+=" --build-arg VERSION=$image_tag"
  build_cmd+=" --build-arg COMMIT=$(git rev-parse HEAD 2>/dev/null || echo unknown)"
  build_cmd+=" -t $image_ref"
  build_cmd+=" -t ${IMAGE_REPO}:latest"
  build_cmd+=" -f cmd/${AGENT_NAME}/Dockerfile ."
  run_or_print "$build_cmd"
fi

# Phase 2: save (On operator)
step "On operator: docker save (cached by image id)"
save_if_changed "$image_ref" "$image_tar"
expected_id="$(cat "${image_tar}.id" 2>/dev/null || echo "")"

# Phase 3: ship config + compose + the image tar (operator → remote)
step "On operator → remote: rsync configs + image tar to $install_dir"

# One staging directory, one rsync: everything the remote needs beside the
# image is rendered here first, so the transfer is a single round trip.
#
# The modes are deliberate. The operator key is a secret and must land 0600,
# and rsync -a carrying the staged mode is the only portable way to get it
# there — macOS's openrsync rejects --chmod=F600. The other two files shipped
# from mktemp (0600) before this, so pin the whole directory to match rather
# than let the umask quietly relax them to 0644 on every deployed host. 0755
# on the directory itself keeps $install_dir at the mode `mkdir -p` gave it:
# with a trailing slash on the source, rsync applies the source root's
# attributes to the destination root.
stage_dir="$(mktemp -d -t "${AGENT_NAME}.stage.XXXXXX")"
trap 'rm -rf "$stage_dir"' EXIT

render_agent_toml   > "$stage_dir/agent.toml"
render_compose_yaml > "$stage_dir/docker-compose.yml"
if [[ -n "$operator_key" ]]; then
  cp "$operator_key" "$stage_dir/operator.key"
fi
chmod 600 "$stage_dir"/*
chmod 755 "$stage_dir"

remote_exec "mkdir -p $install_dir $install_dir/data"
# The trailing slash on the source is load-bearing: without it rsync creates
# $install_dir/<staging-dir-name>/ and the agent keeps running against its old
# agent.toml, with nothing anywhere reporting an error.
run_or_print "rsync -avz '$stage_dir/' '$host:$install_dir/'"
# The image tar ships separately: save_if_changed keys its skip on the
# ${image_tar}.id sidecar in build/, so folding a multi-hundred-MB file into
# the staging dir would mean copying it on every run.
run_or_print "rsync -avz '$image_tar' '$host:$install_dir/${AGENT_NAME}.image.tar'"

# Phase 4: load (On remote)
step "On remote: docker load (skipped if image already loaded)"
load_if_missing "$image_ref" "$install_dir/${AGENT_NAME}.image.tar" "$expected_id"
remote_exec "docker tag $image_ref ${IMAGE_REPO}:latest"

# Phase 5: run (On remote)
step "On remote: docker compose up -d"
# The explicit rm first: compose will not recreate a container it considers
# up-to-date, so a config-only change to a mounted file would otherwise leave
# the old process running.
remote_exec "docker rm -f $AGENT_NAME 2>/dev/null || true"
remote_exec "docker compose -f $install_dir/docker-compose.yml up -d"

# Phase 6: verify (On operator) — smoke-test over loopback on the remote.
step "On remote: smoke test (healthz + agent card over loopback via ssh)"
if [[ "$dry_run" == "1" ]]; then
  info "[dry-run] would ssh $host curl -> http://127.0.0.1:${AGENT_PORT}/healthz + agent card"
else
  # The agent dials the chain gRPC and finishes an initial markets-cache
  # refresh before serving; give it a few seconds to come up.
  healthy=""
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    code=$(ssh -o BatchMode=yes "$host" \
      "curl -sS -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:${AGENT_PORT}/healthz" \
      2>/dev/null || echo 000)
    if [[ "$code" == "200" ]]; then healthy="1"; break; fi
    sleep 2
  done
  if [[ -z "$healthy" ]]; then
    info "healthz on :${AGENT_PORT} did not answer 200. Check logs with:"
    info "  ssh $host 'docker logs $AGENT_NAME --tail=80'"
    info "Common cause: the gRPC/RPC endpoints in agent.toml are not reachable"
    info "from inside the container."
    fail "smoke test failed for $AGENT_NAME"
  fi
  skills=$(ssh -o BatchMode=yes "$host" \
    "curl -sS --max-time 5 http://127.0.0.1:${AGENT_PORT}/.well-known/agent-card.json" \
    2>/dev/null | { command -v jq >/dev/null 2>&1 && jq -r '.skills | length' || cat; } || echo "")
  if [[ "$skills" =~ ^[0-9]+$ ]]; then
    pass "$AGENT_NAME :${AGENT_PORT} — /healthz 200, card served ($skills skills)"
  else
    # jq may be missing on the operator — the card body already proves the
    # endpoint answers; don't fail the deploy over the count.
    pass "$AGENT_NAME :${AGENT_PORT} — /healthz 200, card fetched (skill count unverified)"
  fi
fi

step "Done — $AGENT_NAME $image_tag running on $host (:${AGENT_PORT}, advertised at $agent_public_url)"
