// List (or delete with --delete) kagent Sessions (AgentInstances) for one AgentTemplate.
//   node scripts/kagent-sessions.mjs devops-droid [--delete]
import os from 'node:os';
import path from 'node:path';

const lib = process.env.KAGENT_SUBSTRATE_LIB || path.join(os.homedir(), 'kagent-enterprise-substrate/lib');
const { client } = await import(path.join(lib, 'grpcweb.mjs'));
const { tokenSource } = await import(path.join(lib, 'enterprise.mjs'));
const INST = 'kagent.api.v1alpha1.AgentInstanceService/';

const [name, flag] = process.argv.slice(2);
const tokens = tokenSource({ uiBase: process.env.KAGENT_UI || 'http://127.0.0.1:8001', token: process.env.KAGENT_TOKEN });
const api = client(process.env.KAGENT_API || 'http://127.0.0.1:8083', tokens);
const all = [];
for (let token = '', n = 0; n < 50; n++) {
  const r = await api.unary(INST + 'ListAgentInstances', { all_creators: true, page: { limit: 100, page_token: token } });
  all.push(...(r.agent_instances ?? []));
  token = r.page?.next_page_token;
  if (!token) break;
}
const mine = all.filter(i => i.agent_template?.name === name || i.name?.endsWith(name));
for (const i of mine) {
  console.log(i.id, i.name, JSON.stringify(i.agent_template ?? {}));
  if (flag === '--delete') await api.unary(INST + 'DeleteAgentInstance', { agent_instance_id: i.id }).then(() => console.log('  deleted'), e => console.log('  delete failed:', e.message));
}
console.log(`${mine.length} session(s)`);
