"""Send one message to a droid harness over A2A gRPC and print the stream.

usage: python scripts/invoke.py [--addr localhost:50080] [--context ID] "prompt"
"""
import argparse, asyncio, json, uuid

import grpc
from a2a.types import a2a_pb2, a2a_pb2_grpc
from google.protobuf.json_format import MessageToDict


async def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--addr", default="localhost:50080")
    ap.add_argument("--context", default=None)
    ap.add_argument("prompt")
    a = ap.parse_args()
    ctx = a.context or str(uuid.uuid4())
    async with grpc.aio.insecure_channel(a.addr) as ch:
        stub = a2a_pb2_grpc.A2AServiceStub(ch)
        req = a2a_pb2.SendMessageRequest(message=a2a_pb2.Message(
            message_id=str(uuid.uuid4()), context_id=ctx, role=a2a_pb2.Role.ROLE_USER,
            parts=[a2a_pb2.Part(text=a.prompt)]))
        print(f"context={ctx}")
        async for ev in stub.SendStreamingMessage(req):
            d = MessageToDict(ev)
            kind = next(iter(d))
            body = d[kind]
            if kind == "statusUpdate":
                st = body["status"]
                parts = st.get("message", {}).get("parts", [])
                for p in parts:
                    if "text" in p:
                        print(f"[{st['state']}] {p['text'][:400]}")
                    else:
                        print(f"[{st['state']}] {p.get('metadata', {}).get('kagent_type')}: {json.dumps(p.get('data'))[:300]}")
                if not parts:
                    print(f"[{st['state']}]")
            elif kind == "artifactUpdate":
                print("[artifact]", "".join(p.get("text", "") for p in body["artifact"]["parts"])[:2000])
            else:
                print(f"[{kind}]")


asyncio.run(main())
