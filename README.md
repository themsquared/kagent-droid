# kagent-droid

Factory's Droid as a kagent `byo` agent for DevOps and platform engineering
work.

A small Go front serves kagent's BYO contract (A2A gRPC `lf.a2a.v1.A2AService`
on :80, `GET /readyz` on :8081). Each A2A turn runs one
`droid exec -o stream-json` process. A kagent Session maps to one droid session
ID, and later turns continue it with `-s`. The AgentTemplate supplies the system
prompt, model and MCP tools, so the image does not change per agent.

| Path | What |
| --- | --- |
| `front/config.go` | Reads `KAGENT_CONFIG_JSON` (the flat alpha4 shape and the newer `runtime`/`agent` shape), writes `~/.factory/mcp.json`, maps model IDs, wires the Factory credential |
| `front/executor.go` | One `droid exec` per turn; stream-json events become A2A status updates (`function_call` / `function_response` DataParts), artifact, final state |
| `front/tasks.go` | Adopts task IDs that kagent creates before it sends the message |
| `front/main.go` | gRPC + readyz servers, agent card |
| `Dockerfile` | Static Go front + droid 0.242.0 (checksum verified) + kubectl 1.34.1 + helm 3.19.0 + git/jq/rg on debian:trixie-slim |
| `deploy/` | Harness (`byo`), DevOps AgentTemplate, Factory ModelConfig |
| `scripts/invoke.py` | Talk A2A gRPC straight to a harness (needs `a2a-sdk[grpc]`) |
| `scripts/kagent-invoke.mjs`, `kagent-sessions.mjs` | Invoke, list and delete Sessions through the kagent Enterprise API |

## Why the front is Go, and why droid runs per turn

Substrate checkpoints the Actor right after `/readyz` passes, and again after
every turn, then restores it from that snapshot for the next request. On this
cluster a Python process did not survive the restore: readyz was refused, and
the router's 5s ResumeActor deadline cancelled every attempt. Probe images (in
`../restore-probe/`) isolated the cause:

| Probe (same python:3.12-slim-trixie image) | Restore |
| --- | --- |
| Static Go readyz server | 2.0s, heartbeat resumes |
| Python asyncio readyz, no grpcio | fails, no heartbeat after restore |
| Python blocking readyz on a thread | fails |
| Python asyncio + `GLIBC_TUNABLES=glibc.pthread.rseq=0` | fails |

So the only process alive at checkpoint time is the Go front. droid (a glibc
binary) runs only during a turn, and its process group is killed when the turn
ends. With this design the Actor checkpoints in ~0.26s and restores in 0.4-0.6s.

## Mapping

| AgentTemplate | Droid |
| --- | --- |
| `systemPrompt` | `--append-system-prompt` |
| `modelConfig.model` | `-m`, mapped to Factory IDs (`claude-haiku-4-5` → `claude-haiku-4-5-20251001`), or `DROID_MODEL` on the Harness |
| `tools[].mcp` (HTTP/SSE) | `~/.factory/mcp.json`; `requireApproval` servers are left out (no HITL path yet) |
| Harness `DROID_AUTONOMY` | `--auto` (default `high`; the gVisor sandbox is the boundary) |

Factory auth uses Substrate credential injection, and droid talks to two
Factory hosts, so both need an entry:

| Host | Declared by | Credential |
| --- | --- | --- |
| `api.factory.ai` | `deploy/factory-modelconfig.yaml` (OpenAI-type ModelConfig, `baseUrl: https://api.factory.ai`) | Secret `factory-api-key`, key `FACTORY_API_KEY` = `fk-...` |
| `prem.factory.ai` | `deploy/factory-egress.yaml` (RemoteMCPServer used only for egress) | Secret `factory-prem-auth`, key `Authorization` = `Bearer fk-...` |

kagent Enterprise rejects Secret-backed Harness env, wildcards like
`https://*.factory.ai` ("credential injection requires an exact DNS
hostname"), and has no egress allowlist field. The Actor only sees the
placeholder `kagent-credential-injected` (in `OPENAI_API_KEY`), which the front
passes to droid as `FACTORY_API_KEY`. The gateway swaps it for the real header
on each declared host. `telemetry.factory.ai` stays blocked, which droid
tolerates.

The sandbox has no kubeconfig and egress only reaches declared hosts, so
`kubectl` inside the Droid cannot reach a cluster. Cluster access comes from
kagent-tools 0.3.0 installed read-only (`deploy/k8s-tools-values.yaml`: a
get/list/watch ClusterRole without Secrets, plus `--read-only`, with the k8s,
helm and utils providers), attached as the `k8s-tools` RemoteMCPServer
(`deploy/k8s-tools-mcp.yaml`). The chart's default metrics port collides with
the MCP port, so the values file moves metrics to 8085.

State that must outlive a turn lives on the Actor's `/data` DurableDir:
droid's `HOME` (session store, `mcp.json`), the workspace (`/data/workspace`)
and the kagent-context to droid-session map (`/data/droid-sessions.json`).
Process memory was not carried from one turn to the next.

## Status (2026-10-16, kind-kagent-ent: kagent-enterprise 1.0.0-alpha4, Substrate 0.2.0-beta5)

| Check | Result |
| --- | --- |
| Local: two turns on one context, kagent-style pre-assigned task IDs | Pass, turn 2 remembers turn 1 |
| Golden snapshot | Pass, checkpoint 0.26s |
| Turn 1 through kagent, real Factory key | Pass: droid answered (created `/data/workspace/notes.txt`) |
| Turn 2 on the same `--actor` | Pass: continued the same droid session and read the file back |
| "List the namespaces you can see" | Pass: 8 namespaces through k8s-tools, matching `kubectl get ns` |

## Deploy

All commands target the `kind-kagent-ent` context. Run them from this folder.

```bash
kubectl --context kind-kagent-ent -n kagent create secret generic factory-api-key --from-literal=FACTORY_API_KEY=fk-...
{ printf 'Bearer '; kubectl --context kind-kagent-ent -n kagent get secret factory-api-key -o jsonpath='{.data.FACTORY_API_KEY}' | base64 -d; } | kubectl --context kind-kagent-ent -n kagent create secret generic factory-prem-auth --from-file=Authorization=/dev/stdin
helm --kube-context kind-kagent-ent upgrade --install droid-k8s-tools oci://ghcr.io/kagent-dev/tools/helm/kagent-tools --version 0.3.0 -n kagent -f deploy/k8s-tools-values.yaml --wait
kubectl --context kind-kagent-ent apply -f deploy/factory-modelconfig.yaml -f deploy/factory-egress.yaml -f deploy/k8s-tools-mcp.yaml -f deploy/harness.yaml -f deploy/devops-droid.yaml
kubectl --context kind-kagent-ent -n kagent port-forward svc/kagent-controller 8083:8083 &
kubectl --context kind-kagent-ent -n kagent port-forward svc/kagent-ui 8001:8080 &
node scripts/kagent-invoke.mjs devops-droid "List the namespaces you can see"
node scripts/kagent-invoke.mjs devops-droid "Follow-up question" --actor ai-<id from the first call>
```

After changing either Secret, restart the gateway's credential cache:
`kubectl --context kind-kagent-ent -n ate-system rollout restart deploy/k8s-credential-provider deploy/atenet-egress`.

The Harness image is pinned by digest at `172.20.0.8:5000` (the `kind-registry`
container on the `kind` network). For another cluster, push the image to a
registry the Substrate workers can pull from and update the digest.

Cleanup: `node scripts/kagent-sessions.mjs devops-droid --delete`. A session
that fails to resume can pin a Substrate worker; recover with
`kubectl --context kind-kagent-ent -n kagent delete pod -l ate.dev/worker-pool=kagent-default`.

If invokes fail with `autoauth token: HTTP 502`, the kagent controller is down.
On this lab that has meant the Docker VM is out of CPU (see memory notes):
the controller loses its leader lease, and Postgres and ate-api-server fail
their 1s probes.

## Run locally

```bash
cd front && go build -o /tmp/droid-harness . && cd ..
KAGENT_CONFIG_JSON='{"model":{"model":"claude-haiku-4-5"},"instruction":"Be brief."}' \
  DROID_HARNESS_PORT=50081 DROID_HARNESS_READYZ_PORT=58082 /tmp/droid-harness
python scripts/invoke.py --addr localhost:50081 --context demo "Run uname -a and tell me the OS"
```

## Open items

- Human-in-the-loop for `requireApproval` MCP servers (A2A input-required).
- A2A task history lives in the front's in-memory store, not in the kagent
  controller, so the kagent UI may not show past turns for this agent.
- Write access for the Droid (k8s-tools is read-only on purpose; flip
  `rbac.readOnly` and drop `--read-only` only with human approval in the loop).
