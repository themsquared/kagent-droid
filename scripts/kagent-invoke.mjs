// Invoke a kagent Enterprise agent through the controller API (the UI's path):
// create a Session (AgentInstance) and stream one A2A turn to it.
//
//   kubectl port-forward -n kagent svc/kagent-controller 8083:8083 &
//   kubectl port-forward -n kagent svc/kagent-ui 8001:8080 &
//   node scripts/kagent-invoke.mjs devops-droid "prompt" [--actor ai-<id>]
//
// Reuses the kagent-enterprise client from github.com/themsquared/kagent-enterprise-substrate
// (KAGENT_SUBSTRATE_LIB, default ~/kagent-enterprise-substrate/lib).
import os from 'node:os';
import path from 'node:path';

const lib = process.env.KAGENT_SUBSTRATE_LIB || path.join(os.homedir(), 'kagent-enterprise-substrate/lib');
const { enterprise } = await import(path.join(lib, 'enterprise.mjs'));

const args = process.argv.slice(2);
const ai = args.indexOf('--actor');
const actor = ai >= 0 ? args.splice(ai, 2)[1] : undefined;
const [name, prompt] = args;
if (!name || !prompt) { console.error('usage: kagent-invoke.mjs <agentTemplate> "<prompt>" [--actor ai-<id>]'); process.exit(2); }

const ent = enterprise({
  apiBase: process.env.KAGENT_API || 'http://127.0.0.1:8083',
  uiBase: process.env.KAGENT_UI || 'http://127.0.0.1:8001',
  token: process.env.KAGENT_TOKEN,
});
const r = await ent.converse('kagent', name, prompt, {
  actor, onStart: a => console.error(`session actor: ${a}`),
});
console.log(JSON.stringify({ ok: r.ok, ms: r.ms, actor: r.actor }));
console.log(r.text);
process.exit(r.ok ? 0 : 1);
