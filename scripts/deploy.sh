#!/usr/bin/env bash
#
# scripts/deploy.sh — install the svpchain perps agent onto a remote SSH host
# as a docker container.
#
# This agent is the intermediary: it accepts a task under a re-delegable
# SVP-DT credential, narrows it, and hands the execution leg to a downstream
# agent. It serves on :8081, advertised at <public-url>/research. It is deployed independently: its
# sibling agents (research, evm, lending) each own their own repo and script,
# so nothing here knows or cares about them.
#
# Flow: build (vendored, so the go.mod replace to ../svpagent/protocol never
# leaves the operator) → docker save (cached by image id) → rsync the tar +
# agent.toml (+ operator.key at 0600 when given) + docker-compose.yml to
# ~/svpchain-research-agent → docker load → docker compose up -d → smoke-test
# /healthz and the agent card over loopback.
#
# The operator key turns delegated execution on. It must be DISTINCT from every
# other agent's: an agent's on-chain id derives from its key and
# agent_self_register publishes a hash of this binary's own card, so a shared
# key makes two agents collide on one registry record.
#
# The remote needs only docker + the compose v2 plugin reachable by the ssh
# user without sudo. Auth state is in-memory, so a redeploy wipes it; the
# transfer-out caps persist on the data volume.
#
# Required:
#   --host user@hostname           SSH target.            SVPCHAIN_DEPLOY_HOST
#
# Common options (run --help for the full list — chain endpoints, EVM family,
# faucet, limits, bridge routes):
#   --public-url <url>             Base URL; this agent advertises <base>/research.
#   --operator-key-file <path>     LOCAL hex eth_secp256k1 key, shipped 0600
#                                  beside the config. Unset → keyless, and the
#                                  execution skills refuse with a reason.
#   --image-tag <tag>              Default <git-short-sha>.
#   --install-dir <path>           Default ~/svpchain-research-agent on remote.
#   --print-config / --print-compose / --print-routes / --print-nginx
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

mode="install"        # install | uninstall | print-config | print-compose | print-routes

host=""
chain_id="${SVPCHAIN_CHAIN_ID:-svp-2517-1}"
grpc_addr="${SVPCHAIN_GRPC_ADDR:-127.0.0.1:9090}"
comet_rpc="${SVPCHAIN_COMET_RPC:-http://127.0.0.1:26657}"
indexer="${SVPCHAIN_INDEXER:-http://127.0.0.1:3002}"
agent_chain_id="${SVPCHAIN_AGENT_CHAIN_ID:-}"
agent_chain_rest="${SVPCHAIN_AGENT_CHAIN_REST:-}"
listen_port="${SVPCHAIN_AGENT_LISTEN_PORT:-8081}"
public_url="${SVPCHAIN_AGENT_PUBLIC_URL:-https://agent-testnet.svpchain.org}"
key_dir="${SVPCHAIN_AGENT_KEY_DIR:-}"
operator_key_file="${SVPCHAIN_AGENT_OPERATOR_KEY_FILE:-}"
operator_capabilities="trading"
operator_metadata=""
print_agent="svpchain-research-agent"  # which agent --print-config renders
evm_rpc="${SVPCHAIN_EVM_RPC:-http://127.0.0.1:8545}"
faucet_url="${SVPCHAIN_FAUCET_URL:-https://pre-faucet.svpchain.org}"
evm_uniswap_router="${SVPCHAIN_EVM_UNISWAP_ROUTER:-0xFe7bf2DFd5CB268C6779f1F614638a436Cb701e4}"
evm_wsvp="${SVPCHAIN_EVM_WSVP:-0x771a0a63D8198b7dbea4a16910ff68AB38006531}"
evm_oracle="${SVPCHAIN_EVM_ORACLE:-0xAE351F2dF66DF1A7d2eB0D7574BcDb909E680B56}"
evm_lendora_comptroller="${SVPCHAIN_EVM_LENDORA_COMPTROLLER:-0x0faBb2B5057b14224b04E4cbB217Dd6b275f75a7}"
evm_bridge_addr="${SVPCHAIN_EVM_BRIDGE:-0x78Aca10afd5b28E838ECf0De20c5621CE39D9F4a}"
evm_bridge_routes="${SVPCHAIN_EVM_BRIDGE_ROUTES:-routes.json}"
evm_bridge_routes_src="${SVPCHAIN_EVM_BRIDGE_ROUTES_SRC:-}"
evm_bridge_source_chain_id="${SVPCHAIN_EVM_BRIDGE_SOURCE_CHAIN_ID:-2517}"
evm_foreign_chains="${SVPCHAIN_EVM_FOREIGN_CHAINS:-421614,https://sepolia-rollup.arbitrum.io/rpc,0xB6c74A758E3fA7bf57c22037821f7cA974d0CdfD;11155111,https://ethereum-sepolia-rpc.publicnode.com,0xb9a9937006E886F0Ec145a19634426300dD20a64}"
install_dir="~/svpchain-research-agent"
image_tag=""
platform="linux/amd64"
deposit_max=""
withdraw_max=""
transfer_max=""
daily_withdraw_cap=""
markets_refresh="30s"
skip_build="0"
dry_run="0"
bridge_routes_basename=""
bridge_routes_src_abs=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --host)                   host="$2";              shift 2 ;;
    --chain-id)               chain_id="$2";          shift 2 ;;
    --grpc-addr)              grpc_addr="$2";         shift 2 ;;
    --comet-rpc)              comet_rpc="$2";         shift 2 ;;
    --indexer)                indexer="$2";           shift 2 ;;
    --agent-chain-id)         agent_chain_id="$2";    shift 2 ;;
    --agent-chain-rest)       agent_chain_rest="$2";  shift 2 ;;
    --listen-port)            listen_port="$2";       shift 2 ;;
    --public-url)             public_url="$2";        shift 2 ;;
    --key-dir)                key_dir="$2";           shift 2 ;;
    --operator-key-file)      operator_key_file="$2"; shift 2 ;;
    --print-agent)            print_agent="$2";       shift 2 ;;
    --operator-capabilities)  operator_capabilities="$2"; shift 2 ;;
    --operator-metadata)      operator_metadata="$2"; shift 2 ;;
    --evm-rpc)                evm_rpc="$2";           shift 2 ;;
    --faucet-url)             faucet_url="$2";        shift 2 ;;
    --evm-uniswap-router)     evm_uniswap_router="$2"; shift 2 ;;
    --evm-wsvp)               evm_wsvp="$2";          shift 2 ;;
    --evm-oracle)             evm_oracle="$2";        shift 2 ;;
    --evm-lendora-comptroller) evm_lendora_comptroller="$2"; shift 2 ;;
    --evm-bridge-addr)        evm_bridge_addr="$2";   shift 2 ;;
    --evm-bridge-routes)      evm_bridge_routes="$2"; shift 2 ;;
    --evm-bridge-routes-src)  evm_bridge_routes_src="$2"; shift 2 ;;
    --evm-bridge-source-chain-id) evm_bridge_source_chain_id="$2"; shift 2 ;;
    --evm-foreign-chains)     evm_foreign_chains="$2"; shift 2 ;;
    --install-dir)            install_dir="$2";       shift 2 ;;
    --image-tag)              image_tag="$2";         shift 2 ;;
    --platform)               platform="$2";          shift 2 ;;
    --deposit-max-usdc)       deposit_max="$2";       shift 2 ;;
    --withdraw-max-usdc)      withdraw_max="$2";      shift 2 ;;
    --transfer-max-usdc)      transfer_max="$2";      shift 2 ;;
    --daily-withdraw-cap-usdc) daily_withdraw_cap="$2"; shift 2 ;;
    --markets-refresh)        markets_refresh="$2";   shift 2 ;;
    --skip-build)             skip_build="1";         shift ;;
    --print-config)           mode="print-config";    shift ;;
    --print-compose)          mode="print-compose";   shift ;;
    --print-routes)           mode="print-routes";    shift ;;
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

# ---- the agents co-deployed on this host -----------------------------------
#
# One image (Dockerfile.agents) carries every binary; one container per agent,
# each on its own TCP port. Each category agent serves one slice — perps, evm,
# lending — and the research agent is the intermediary in front of them. Every
# agent reads the same rendered family config (a binary that doesn't register a
# family ignores its config), so the only per-agent differences are the port,
# the advertised URL, and the operator key.
#
# AGENTS is also the deploy order: the research (intermediary) agent sits
# before the perps agent, being the middle hop that re-delegates a narrowed
# credential to a downstream agent (typically perps), which executes on the
# original user's account. Each agent's short key name is what --key-dir looks
# for (<short>.key).
#
# Between them these four cover the whole operation surface; a test in
# internal/wire asserts no operation is lost to the split.
AGENTS=(svpchain-research-agent)

# Every agent this script has ever deployed, retired ones included. The cleanup
# paths (uninstall, and the rm before compose up) iterate this rather than
# AGENTS so a container left behind by an agent that has since been retired is
# removed, instead of lingering on the host running a stale image — `docker
# compose up` will not remove a service the file no longer defines.
#
# svpchain-remote-agents was the full-surface agent, retired once the category
# agents covered its whole surface between them. Its entry stays until every
# host that ran it has been redeployed at least once.
KNOWN_AGENTS=("${AGENTS[@]}")

# Static per-agent facts as case functions, not associative arrays: this script
# must run on the macOS system bash 3.2, which predates `declare -A`.
agent_port() {
  case "$1" in
    svpchain-research-agent) echo 8081 ;;
    *) echo "$listen_port" ;;
  esac
}
agent_keyname() {
  case "$1" in
    svpchain-research-agent) echo research ;;
    *) echo "$1" ;;
  esac
}

# agent_active reports whether an agent is deployed this run. The research
# agent signs the credentials it re-delegates, so it cannot run keyless: it is
# included only when a research key is provided (--key-dir/research.key),
# otherwise skipped so a keyless deploy of the service agents still works.
agent_active() {
  if [[ "$1" == "svpchain-research-agent" && -z "$(agent_key_src "$1")" ]]; then
    return 1
  fi
  return 0
}

# Per-agent operator key path — a mutable map, stored as AGENT_KEY_<short>
# shell vars (indirection via eval, since 3.2 has no associative arrays).
# Empty means keyless. Distinct keys are mandatory when keyed: an agent's id
# derives from its key and agent_self_register hashes that binary's own card,
# so a shared key makes two agents fight over one on-chain record.
set_agent_key() { eval "AGENT_KEY_$(agent_keyname "$1")=\"\$2\""; }
agent_key_src() { eval "printf '%s' \"\${AGENT_KEY_$(agent_keyname "$1")-}\""; }

# ---- shared helpers -------------------------------------------------------

# emit_foreign_chains — emit the [[evm.bridge.foreign_chain]] array-of-tables
# parsed from evm_foreign_chains (";"-separated "chainId,rpcUrl,bridgeAddr"
# triples).
emit_foreign_chains() {
  [[ -z "$evm_foreign_chains" ]] && return 0
  local triple cid rpc addr
  local saved_ifs="$IFS"
  IFS=';'
  for triple in $evm_foreign_chains; do
    IFS="$saved_ifs"
    [[ -z "$triple" ]] && continue
    IFS=',' read -r cid rpc addr <<<"$triple"
    if [[ -z "$cid" || -z "$rpc" || -z "$addr" ]]; then
      fail "--evm-foreign-chains: malformed triple \"$triple\" (want chainId,rpcUrl,bridgeAddr)"
    fi
    printf '\n[[evm.bridge.foreign_chain]]\n'
    printf 'chain_id    = %s\n' "$cid"
    printf 'rpc_url     = "%s"\n' "$rpc"
    printf 'bridge_addr = "%s"\n' "$addr"
    IFS=';'
  done
  IFS="$saved_ifs"
}

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

# render_agent_toml [name] [port] [public_url] [key_path] — emit one agent's
# agent.toml on stdout. Every agent reads the same family config; only the
# name (→ data path), port, advertised URL, and operator key vary. Both call
# sites name an agent; the default is a fallback only.
# listen_addr is always 0.0.0.0:<port> inside the
# container; --network host means that's also the host-bound port. Optional
# families mirror internal/config exactly: unset keys → those operations refuse
# at call time.
render_agent_toml() {
  local name="${1:-svpchain-research-agent}"
  local port="${2:-$(agent_port "$name")}"
  # Advertised URL: the base plus this agent's path segment (/research,
  # /perps, /evm, /lending) — a reverse proxy routes each path to that agent.
  local pub="${3:-${public_url}/$(agent_keyname "$name")}"
  # A 4th arg (even empty) is authoritative — the ship loop passes each agent's
  # key explicitly, including "" for a keyless one. With none, fall back to
  # whatever --key-dir resolved for this agent, so --print-config previews the
  # [operator] block exactly as the deploy would render it.
  local keypath
  if [[ $# -ge 4 ]]; then keypath="$4"; else keypath="$(agent_key_src "$name")"; fi

  cat <<EOF
# Auto-generated by scripts/deploy.sh — do not edit by hand.
# Agent: ${name}

listen_addr      = "0.0.0.0:${port}"
public_url       = "${pub}"
broadcast_mode   = "server"
EOF
  [[ -n "$faucet_url" ]] && echo "faucet_base_url         = \"${faucet_url}\""
  # Persist per-symbol transfer-out caps on this agent's own writable data
  # volume (the config dir holds only read-only mounts) — see
  # render_compose_yaml. Per-agent so two co-deployed agents that both write
  # caps never share a file between agents.
  echo "transfer_out_cap_path   = \"/var/lib/${name}/transfer-out-caps.json\""
  cat <<EOF

[dex_chain]
id               = "${chain_id}"
grpc_addr        = "${grpc_addr}"
comet_rpc_url    = "${comet_rpc}"
indexer_base_url = "${indexer}"
EOF
  [[ -n "$evm_rpc" ]] && echo "evm_rpc_url      = \"${evm_rpc}\""
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
  # Per-protocol contract bindings on the DEX chain's EVM side; each family
  # renders only when configured, mirroring internal/config's optionality.
  if [[ -n "$evm_uniswap_router" ]]; then
    echo ""
    echo "[evm.swap]"
    echo "uniswap_router_addr = \"${evm_uniswap_router}\""
    echo "wsvp_addr           = \"${evm_wsvp}\""
  fi
  if [[ -n "$evm_oracle" ]]; then
    echo ""
    echo "[evm.oracle]"
    echo "feed_addr = \"${evm_oracle}\""
  fi
  if [[ -n "$evm_lendora_comptroller" ]]; then
    echo ""
    echo "[evm.lendora]"
    echo "comptroller_addr = \"${evm_lendora_comptroller}\""
  fi
  if [[ -n "$evm_bridge_addr" && -n "$evm_bridge_routes" && -n "$evm_bridge_source_chain_id" ]]; then
    echo ""
    echo "[evm.bridge]"
    echo "addr            = \"${evm_bridge_addr}\""
    echo "routes_path     = \"${evm_bridge_routes}\""
    echo "source_chain_id = ${evm_bridge_source_chain_id}"
    emit_foreign_chains
  elif [[ -n "$evm_bridge_routes" ]]; then
    echo "# WARNING: --evm-bridge-routes set but evm_bridge_addr / evm_bridge_source_chain_id are empty;" >&2
    echo "#          bridge omitted (config requires all three)." >&2
  fi
  cat <<EOF

[cache]
markets_refresh = "${markets_refresh}"
EOF
  if [[ -n "${deposit_max}${withdraw_max}${transfer_max}${daily_withdraw_cap}" ]]; then
    echo ""
    echo "[limits]"
    [[ -n "$deposit_max"        ]] && echo "deposit_max_usdc        = ${deposit_max}"
    [[ -n "$withdraw_max"       ]] && echo "withdraw_max_usdc       = ${withdraw_max}"
    [[ -n "$transfer_max"       ]] && echo "transfer_max_usdc       = ${transfer_max}"
    [[ -n "$daily_withdraw_cap" ]] && echo "daily_withdraw_cap_usdc = ${daily_withdraw_cap}"
  fi
  # The operator key turns this agent's delegated execution on. key_file is
  # left relative ("operator.key") on purpose — internal/config resolves it
  # against the agent.toml directory, so it points at the file mounted beside
  # the config.
  if [[ -n "$keypath" ]]; then
    cat <<EOF

[operator]
key_file     = "operator.key"
capabilities = $(emit_operator_capabilities)
metadata     = "${operator_metadata}"
EOF
  fi
}

# render_routes_json — the SVPBridge route registry, identical to the one the
# MCP deploy ships (the two services read the same whitelist). Zero addresses
# denote the native coin; decimals are the source asset's.
render_routes_json() {
  cat <<'ROUTES'
[
  {"srcChain":"arbitrum_sepolia","srcChainId":421614,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0x0000000000000000000000000000000000000000","targetToken":"0x1c12dbda863900c680a3836c53d408feaf63f0ba","symbol":"WETH","decimals":18},
  {"srcChain":"arbitrum_sepolia","srcChainId":421614,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0x7a8EcFa70374c1B8702CB98aaf23dE19675981d6","targetToken":"0x0000000000000000000000000000000000000000","symbol":"SVP","decimals":18},
  {"srcChain":"arbitrum_sepolia","srcChainId":421614,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0xc2bda8290a2e01984da81acf7e2d6ec9b14d7b10","targetToken":"0x8787384b8640f6e9c30e94585d3d62b03f80a5df","symbol":"WBNB","decimals":18},
  {"srcChain":"arbitrum_sepolia","srcChainId":421614,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0xd10d01ebf3cb825da77a025b1d861e7ae5370c20","targetToken":"0x6c22ceb0852bd7781b57574aaa5de0f22cd44162","symbol":"WBTC","decimals":8},
  {"srcChain":"arbitrum_sepolia","srcChainId":421614,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0x75faf114eafb1bdbe2f0316df893fd58ce46aa4d","targetToken":"0x732f6ea7afd5edc02e7ba052075dd0780e285489","symbol":"USDC","decimals":6},
  {"srcChain":"arbitrum_sepolia","srcChainId":421614,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0xfa9857651febd22c0a76c958adb25b4af0370688","targetToken":"0x013a61e622e6abfcab64f52d274c3fc0aa37f951","symbol":"USDV","decimals":6},
  {"srcChain":"sepolia","srcChainId":11155111,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0x0000000000000000000000000000000000000000","targetToken":"0x1c12dbda863900c680a3836c53d408feaf63f0ba","symbol":"WETH","decimals":18},
  {"srcChain":"sepolia","srcChainId":11155111,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0x16B065D7519D5C1c53eff6ed5AE732E90d602A00","targetToken":"0x0000000000000000000000000000000000000000","symbol":"SVP","decimals":18},
  {"srcChain":"sepolia","srcChainId":11155111,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0x1c7d4b196cb0c7b01d743fbc6116a902379c7238","targetToken":"0x732f6ea7afd5edc02e7ba052075dd0780e285489","symbol":"USDC","decimals":6},
  {"srcChain":"sepolia","srcChainId":11155111,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0x93e719f5458d112804122952033103f2eb349eac","targetToken":"0x013a61e622e6abfcab64f52d274c3fc0aa37f951","symbol":"USDV","decimals":6},
  {"srcChain":"sepolia","srcChainId":11155111,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0x9d45d6a420fbaf77a46a4822ef967d62a69dc7f8","targetToken":"0x6c22ceb0852bd7781b57574aaa5de0f22cd44162","symbol":"WBTC","decimals":8},
  {"srcChain":"sepolia","srcChainId":11155111,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0xf174007a92ae5cdfecfa85c94c5105e4851734d6","targetToken":"0x8787384b8640f6e9c30e94585d3d62b03f80a5df","symbol":"WBNB","decimals":18},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"arbitrum_sepolia","targetChainId":421614,"srcToken":"0x0000000000000000000000000000000000000000","targetToken":"0x7a8EcFa70374c1B8702CB98aaf23dE19675981d6","symbol":"SVP","decimals":18},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"arbitrum_sepolia","targetChainId":421614,"srcToken":"0x013a61e622e6abfcab64f52d274c3fc0aa37f951","targetToken":"0xfa9857651febd22c0a76c958adb25b4af0370688","symbol":"USDV","decimals":6},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"arbitrum_sepolia","targetChainId":421614,"srcToken":"0x1c12dbda863900c680a3836c53d408feaf63f0ba","targetToken":"0x0000000000000000000000000000000000000000","symbol":"WETH","decimals":18},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"arbitrum_sepolia","targetChainId":421614,"srcToken":"0x6c22ceb0852bd7781b57574aaa5de0f22cd44162","targetToken":"0xd10d01ebf3cb825da77a025b1d861e7ae5370c20","symbol":"WBTC","decimals":8},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"arbitrum_sepolia","targetChainId":421614,"srcToken":"0x732f6ea7afd5edc02e7ba052075dd0780e285489","targetToken":"0x75faf114eafb1bdbe2f0316df893fd58ce46aa4d","symbol":"USDC","decimals":6},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"arbitrum_sepolia","targetChainId":421614,"srcToken":"0x8787384b8640f6e9c30e94585d3d62b03f80a5df","targetToken":"0xc2bda8290a2e01984da81acf7e2d6ec9b14d7b10","symbol":"WBNB","decimals":18},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"sepolia","targetChainId":11155111,"srcToken":"0x0000000000000000000000000000000000000000","targetToken":"0x16B065D7519D5C1c53eff6ed5AE732E90d602A00","symbol":"SVP","decimals":18},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"sepolia","targetChainId":11155111,"srcToken":"0x013a61e622e6abfcab64f52d274c3fc0aa37f951","targetToken":"0x93e719f5458d112804122952033103f2eb349eac","symbol":"USDV","decimals":6},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"sepolia","targetChainId":11155111,"srcToken":"0x1c12dbda863900c680a3836c53d408feaf63f0ba","targetToken":"0x0000000000000000000000000000000000000000","symbol":"WETH","decimals":18},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"sepolia","targetChainId":11155111,"srcToken":"0x6c22ceb0852bd7781b57574aaa5de0f22cd44162","targetToken":"0x9d45d6a420fbaf77a46a4822ef967d62a69dc7f8","symbol":"WBTC","decimals":8},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"sepolia","targetChainId":11155111,"srcToken":"0x732f6ea7afd5edc02e7ba052075dd0780e285489","targetToken":"0x1c7d4b196cb0c7b01d743fbc6116a902379c7238","symbol":"USDC","decimals":6},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"sepolia","targetChainId":11155111,"srcToken":"0x8787384b8640f6e9c30e94585d3d62b03f80a5df","targetToken":"0xf174007a92ae5cdfecfa85c94c5105e4851734d6","symbol":"WBNB","decimals":18}
]
ROUTES
}

# render_compose_yaml — emit the single docker-compose.yml that runs every
# agent from the one shared image. One service per agent: same image, its own
# command (which binary), config mount, data volume, and TCP port. Volumes use
# absolute host paths so `docker compose up -d` works from any directory. Each
# agent's files live under ${install_dir}/<name>/; its operator key and the
# bridge routes (when present) are mounted read-only beside its config so the
# config-dir-relative resolution finds them.
render_compose_yaml() {
  echo "# Auto-generated by scripts/deploy.sh — do not edit by hand."
  echo "services:"
  local name port cmd
  for name in "${AGENTS[@]}"; do
    agent_active "$name" || continue
    port="$(agent_port "$name")"
    # ★ ARGS ONLY — no binary path. This image declares
    # ENTRYPOINT ["/bin/svpchain-research-agent"], and compose `command:`
    # overrides CMD, not ENTRYPOINT. Naming the binary here (as the old shared
    # multi-binary image required, since it had no ENTRYPOINT) would launch
    # `svpchain-research-agent svpchain-research-agent -config …` and die on flag
    # parsing.
    cmd="[\"-config\", \"/etc/${name}/agent.toml\", \"-listen\", \"0.0.0.0:${port}\", \"-public-url\", \"${public_url}/$(agent_keyname "$name")\"]"
    cat <<EOF
  ${name}:
    image: ${image_ref}
    container_name: ${name}
    restart: unless-stopped
    command: ${cmd}
    # network_mode: host — the listener binds 0.0.0.0:${port} (compose
    # \`ports:\` is ignored in host mode; the port lives in agent.toml).
    network_mode: host
    volumes:
      - ${install_dir}/agent.toml:/etc/${name}/agent.toml:ro
      - ${install_dir}/data:/var/lib/${name}
EOF
    # Explicit ifs, not `[[ … ]] && echo`: a false test as the last command
    # would make the function return non-zero, and under `set -e` the
    # `render_compose_yaml > file` call site would exit the script silently.
    if [[ -n "$(agent_key_src "$name")" ]]; then
      echo "      - ${install_dir}/operator.key:/etc/${name}/operator.key:ro"
    fi
    # The bridge routes ride only with the evm agent, the one that serves the
    # bridge; scoped rather than mounted everywhere to keep the mounts honest.
    if [[ -n "$bridge_routes_basename" && "$name" == "svpchain-evm-agent" ]]; then
      echo "      - ${install_dir}/${bridge_routes_basename}:/etc/${name}/${bridge_routes_basename}:ro"
    fi
  done
}

require_install_args() {
  [[ -n "$host" ]] || fail "--host is required (or set SVPCHAIN_DEPLOY_HOST)"
}

# validate_hex_key — a file must look like a 32-byte hex operator key.
validate_hex_key() {
  grep -Eq '^(0x)?[0-9a-fA-F]{64}[[:space:]]*$' "$1" \
    || fail "operator key '$1' does not look like a 32-byte hex key"
}

# resolve_operator_keys — find this agent's operator key, from
# --operator-key-file or from --key-dir/<short>.key. Without one the agent runs
# keyless: it advertises execution but refuses with a reason.
#
# The key must be distinct from every other agent's — an agent's on-chain id
# derives from it and agent_self_register hashes this binary's own card, so a
# shared key makes two agents collide on one registry record. With the agents in
# separate repos nothing can check that here; it is an operational rule.
resolve_operator_keys() {
  local name short src
  if [[ -n "$operator_key_file" ]]; then
    src="$operator_key_file"
    [[ "$src" = /* ]] || src="$(pwd)/$src"
    [[ -f "$src" ]] || fail "--operator-key-file '$src' was not found"
    validate_hex_key "$src"
    set_agent_key "${AGENTS[0]}" "$src"
  fi
  if [[ -n "$key_dir" ]]; then
    for name in "${AGENTS[@]}"; do
      short="$(agent_keyname "$name")"
      src="${key_dir%/}/${short}.key"
      [[ "$src" = /* ]] || src="$(pwd)/$src"
      if [[ -f "$src" ]]; then
        validate_hex_key "$src"
        set_agent_key "$name" "$src"
      fi
    done
  fi

  # Distinct-key guard, by key content (not just path): the same key under two
  # names is still one identity. Pairwise — there are only four agents.
  local i j ni nj ci cj
  for (( i = 0; i < ${#AGENTS[@]}; i++ )); do
    ni="${AGENTS[$i]}"; ci="$(agent_key_src "$ni")"
    [[ -n "$ci" ]] || continue
    ci="$(tr -d '[:space:]' < "$ci")"
    for (( j = i + 1; j < ${#AGENTS[@]}; j++ )); do
      nj="${AGENTS[$j]}"; cj="$(agent_key_src "$nj")"
      [[ -n "$cj" ]] || continue
      cj="$(tr -d '[:space:]' < "$cj")"
      if [[ "$ci" == "$cj" ]]; then
        fail "agents ${ni} and ${nj} use the same operator key; each agent needs a DISTINCT key (its on-chain id derives from the key and agent_self_register hashes that binary's own card, so a shared key makes them fight over one record)"
      fi
    done
  done
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
# port. That mapping lives in exactly two places: agent_keyname for the segment
# and agent_port for the port — the same two functions that build public_url and
# the compose port, so a route printed here cannot disagree with what deployed.
#
# Nothing installs this. The server block it belongs in owns TLS and the base
# host, which are outside this repo and shared with agents this repo must not
# know about; four scripts racing to edit one nginx file is how you get a
# half-written config on reload. Print it, review it, paste it.
render_nginx_conf() {
  local name="${1:-${AGENTS[0]}}"
  local seg port
  seg="$(agent_keyname "$name")"
  port="$(agent_port "$name")"
  cat <<EOF
# ${name} — generated by scripts/deploy.sh --print-nginx
# Paste into the server block for $(printf '%s' "${public_url#*://}"), then
# \`nginx -t && systemctl reload nginx\`.

# Bare /${seg} would 404: the location below only matches the trailing slash.
location = /${seg} { return 301 /${seg}/; }

location /${seg}/ {
    # Trailing slash strips the /${seg} prefix. The agent binds at root and
    # serves /.well-known/agent-card.json and /invoke there; it only knows
    # about /${seg} as the public_url it advertises inside the card.
    proxy_pass http://127.0.0.1:${port}/;

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
  # Preview one agent's config (default svpchain-research-agent; --print-agent
  # <name> for another), with that agent's port and — when --key-dir supplies
  # it a key — its [operator] block.
  resolve_operator_keys
  render_agent_toml "$print_agent"
  exit 0
fi

# ---- mode: print-compose --------------------------------------------------

if [[ "$mode" == "print-compose" ]]; then
  # Preview the one docker-compose.yml that runs every agent. Uses a
  # placeholder install_dir/image when not resolved, and reflects --key-dir so
  # keyed agents show their operator.key mount.
  resolve_operator_keys
  image_ref="ghcr.io/svpchain/svpchain-research-agent:${image_tag:-<tag>}"
  render_compose_yaml
  exit 0
fi

# ---- mode: print-routes ---------------------------------------------------

if [[ "$mode" == "print-routes" ]]; then
  render_routes_json
  exit 0
fi

# ---- mode: print-nginx ----------------------------------------------------

if [[ "$mode" == "print-nginx" ]]; then
  render_nginx_conf "${AGENTS[0]}"
  exit 0
fi

# ---- mode: uninstall ------------------------------------------------------

if [[ "$mode" == "uninstall" ]]; then
  [[ -n "$host" ]] || fail "--host is required (or set SVPCHAIN_DEPLOY_HOST)"
  step "svpchain agents uninstall on $host"
  resolve_remote_install_dir
  # compose down brings every agent service down together.
  remote_exec "docker compose -f $install_dir/docker-compose.yml down 2>/dev/null || true"
  # Belt-and-braces: remove each agent container by name in case the compose
  # file is gone, then the shared image, then the install dir. Iterates the
  # known set, not the deployed one, so a retired agent's container goes too.
  for name in "${KNOWN_AGENTS[@]}"; do
    remote_exec "docker rm -f $name 2>/dev/null || true"
  done
  remote_exec "sh -c 'docker images --format \"{{.Repository}}:{{.Tag}}\" svpchain-remote-agents 2>/dev/null | xargs -r docker rmi 2>/dev/null || true'"
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

# Per-agent operator keys: resolve local paths (against the operator's CWD,
# before any cd), validate them, and enforce the distinct-key rule. An agent
# with no key runs keyless.
resolve_operator_keys

# Bridge route shipping — same rules as the MCP deploy: with a RELATIVE routes
# path the registry is generated (or overridden via --evm-bridge-routes-src)
# and mounted next to agent.toml; an ABSOLUTE path is operator-managed.
if [[ -n "$evm_bridge_addr" && -n "$evm_bridge_routes" && -n "$evm_bridge_source_chain_id" ]]; then
  case "$evm_bridge_routes" in
    /*)
      info "bridge: evm_bridge_routes is absolute ($evm_bridge_routes) — not auto-shipping; ensure that path exists on $host."
      ;;
    *)
      bridge_routes_basename="$(basename "$evm_bridge_routes")"
      if [[ -n "$evm_bridge_routes_src" ]]; then
        if [[ "$evm_bridge_routes_src" = /* ]]; then
          bridge_routes_src_abs="$evm_bridge_routes_src"
        else
          bridge_routes_src_abs="$(pwd)/$evm_bridge_routes_src"
        fi
        [[ -f "$bridge_routes_src_abs" ]] || fail "--evm-bridge-routes-src '$bridge_routes_src_abs' was not found"
      fi
      ;;
  esac
fi

REPO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "$REPO_DIR"

if [[ -z "$image_tag" ]]; then
  if image_tag="$(git rev-parse --short HEAD 2>/dev/null)"; then :
  else image_tag="dev"; fi
fi
image_ref="ghcr.io/svpchain/svpchain-research-agent:${image_tag}"
image_tar="${REPO_DIR}/build/svpchain-research-agent.image.tar"
mkdir -p "${REPO_DIR}/build"

step "Preflight (operator + remote)"
info "host=$host image=$image_ref platform=$platform"
info "install_dir=$install_dir public_url_base=$public_url"
for name in "${AGENTS[@]}"; do
  if ! agent_active "$name"; then
    info "  ${name} — skipped (needs an operator key; add research.key to --key-dir to deploy it)"
    continue
  fi
  keysrc="$(agent_key_src "$name")"
  if [[ -n "$keysrc" ]]; then
    info "  ${name} :$(agent_port "$name") — key ${keysrc} (execution ON)"
  else
    info "  ${name} :$(agent_port "$name") — keyless (execution refuses with a reason)"
  fi
done
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
  # Vendored build (see Dockerfile.agents): the go.mod replace to
  # ../svpagent/protocol resolves on the operator, and the vendored tree makes
  # the Docker context self-contained. One image carries every agent binary.
  run_or_print "go mod vendor"
  build_cmd="docker build --platform $platform"
  build_cmd+=" --build-arg VERSION=$image_tag"
  build_cmd+=" --build-arg COMMIT=$(git rev-parse HEAD 2>/dev/null || echo unknown)"
  build_cmd+=" -t $image_ref"
  build_cmd+=" -t ghcr.io/svpchain/svpchain-research-agent:latest"
  build_cmd+=" -f cmd/svpchain-research-agent/Dockerfile ."
  run_or_print "$build_cmd"
fi

# Phase 2: save (On operator)
step "On operator: docker save (cached by image id)"
save_if_changed "$image_ref" "$image_tar"
expected_id="$(cat "${image_tar}.id" 2>/dev/null || echo "")"

# Phase 3: ship per-agent config + one compose + one image tar (operator → remote)
step "On operator → remote: rsync configs + image tar to $install_dir"
compose_tmp="$(mktemp -t svpchain-research-agent.compose.XXXXXX)"
routes_tmp=""
declare -a AGENT_TMP=() KEY_TMP=()
trap 'rm -f "$compose_tmp" "$routes_tmp" ${AGENT_TMP[@]+"${AGENT_TMP[@]}"} ${KEY_TMP[@]+"${KEY_TMP[@]}"}' EXIT

render_compose_yaml > "$compose_tmp"

# The bridge route registry is shipped once per agent that serves the bridge.
bridge_routes_ship=""
if [[ -n "$bridge_routes_basename" ]]; then
  if [[ -n "$bridge_routes_src_abs" ]]; then
    bridge_routes_ship="$bridge_routes_src_abs"
  else
    routes_tmp="$(mktemp -t svpchain-research-agent.routes.XXXXXX)"
    render_routes_json > "$routes_tmp"
    bridge_routes_ship="$routes_tmp"
  fi
fi

remote_exec "mkdir -p $install_dir"
for name in "${AGENTS[@]}"; do
  agent_active "$name" || continue
  port="$(agent_port "$name")"
  keysrc="$(agent_key_src "$name")"

  toml_tmp="$(mktemp -t "${name}.toml.XXXXXX")"
  AGENT_TMP+=("$toml_tmp")
  # 4 args pin the per-agent key (empty → keyless), overriding the fallback.
  render_agent_toml "$name" "$port" "${public_url}/$(agent_keyname "$name")" "$keysrc" > "$toml_tmp"

  remote_exec "mkdir -p $install_dir $install_dir/data"
  run_or_print "rsync -avz '$toml_tmp' '$host:$install_dir/agent.toml'"

  # The operator key is a secret: ship a 0600 temp copy — rsync -a preserves
  # permissions where --chmod=F600 is not portable (macOS's openrsync rejects
  # the octal form) — and pin it remotely too.
  if [[ -n "$keysrc" ]]; then
    key_tmp="$(mktemp -t "${name}.key.XXXXXX")"
    KEY_TMP+=("$key_tmp")
    run_or_print "cp '$keysrc' '$key_tmp' && chmod 600 '$key_tmp'"
    run_or_print "rsync -avz '$key_tmp' '$host:$install_dir/operator.key'"
    remote_exec "chmod 600 $install_dir/operator.key"
  fi

  # Bridge routes ride only with the agents whose compose service mounts them.
  if [[ -n "$bridge_routes_ship" && "$name" == "svpchain-evm-agent" ]]; then
    run_or_print "rsync -avz '$bridge_routes_ship' '$host:$install_dir/$bridge_routes_basename'"
  fi
done

run_or_print "rsync -avz '$compose_tmp' '$host:$install_dir/docker-compose.yml'"
run_or_print "rsync -avz '$image_tar' '$host:$install_dir/svpchain-research-agent.image.tar'"

# Phase 4: load (On remote)
step "On remote: docker load (skipped if image already loaded)"
load_if_missing "$image_ref" "$install_dir/svpchain-research-agent.image.tar" "$expected_id"
remote_exec "docker tag $image_ref ghcr.io/svpchain/svpchain-research-agent:latest"

# Phase 5: run (On remote) — one compose up brings every agent service up.
step "On remote: docker compose up -d"
# The known set, not just the deployed one: an agent retired since the last
# deploy still has a container here, and compose will not remove a service it
# no longer defines — it would sit there running a stale image.
for name in "${KNOWN_AGENTS[@]}"; do
  remote_exec "docker rm -f $name 2>/dev/null || true"
done
remote_exec "docker compose -f $install_dir/docker-compose.yml up -d"

# Phase 6: verify (On operator) — smoke-test every agent on its own port.
step "On remote: smoke test (healthz + agent card over loopback via ssh)"
for name in "${AGENTS[@]}"; do
  agent_active "$name" || continue
  port="$(agent_port "$name")"
  if [[ "$dry_run" == "1" ]]; then
    info "[dry-run] would ssh $host curl -> http://127.0.0.1:$port/healthz + agent card ($name)"
    continue
  fi
  # Each agent dials the chain gRPC and (for the perp-pricing profiles)
  # finishes an initial markets-cache refresh before serving; give it a few
  # seconds to come up.
  healthy=""
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    code=$(ssh -o BatchMode=yes "$host" \
      "curl -sS -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:${port}/healthz" \
      2>/dev/null || echo 000)
    if [[ "$code" == "200" ]]; then healthy="1"; break; fi
    sleep 2
  done
  if [[ -z "$healthy" ]]; then
    info "$name: healthz on :$port did not answer 200. Check logs with:"
    info "  ssh $host 'docker logs $name --tail=80'"
    info "Common causes: gRPC/RPC endpoints in agent.toml not reachable from"
    info "the container; the evm/lending agents also need evm_rpc + comptroller."
    fail "smoke test failed for $name"
  fi
  skills=$(ssh -o BatchMode=yes "$host" \
    "curl -sS --max-time 5 http://127.0.0.1:${port}/.well-known/agent-card.json" \
    2>/dev/null | { command -v jq >/dev/null 2>&1 && jq -r '.skills | length' || cat; } || echo "")
  if [[ "$skills" =~ ^[0-9]+$ ]]; then
    pass "$name :$port — /healthz 200, card served ($skills skills)"
  else
    # jq may be missing on the operator — the card body already proves the
    # endpoint answers; don't fail the deploy over the count.
    pass "$name :$port — /healthz 200, card fetched (skill count unverified)"
  fi
done

step "Done — svpchain agents $image_tag running on $host (research :8081, perps :8082, evm :8083, lending :8084)"
