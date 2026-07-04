// Copyright © 2026 Hanzo AI. MIT License.
//
// Live integration test against a running tasksd ZAP endpoint. Skipped unless
// TASKS_ZAP_ADDR is set (e.g. "127.0.0.1:9999"). Run tasksd with:
//   ./tasksd --allow-no-auth start
// then: TASKS_ZAP_ADDR=127.0.0.1:9999 npx vitest run test/integration.test.ts

import { describe, it, expect, afterAll } from "vitest";
import { TasksClient } from "../src/client";
import { Worker } from "../src/worker";
import { proxyActivities } from "../src/workflow";
import { WorkflowStatus } from "../src/client/options";

const ADDR = process.env.TASKS_ZAP_ADDR;
const NS = process.env.TASKS_NAMESPACE ?? "default";
const suite = ADDR ? describe : describe.skip;

suite("integration — live tasksd over ZAP", () => {
  const cleanup: Array<() => Promise<void>> = [];
  afterAll(async () => {
    for (const fn of cleanup) await fn().catch(() => {});
  });

  it("reports health", async () => {
    const client = await TasksClient.connect({ address: ADDR!, namespace: NS });
    cleanup.push(() => client.close());
    const h = await client.health();
    expect(typeof h.status).toBe("string");
  });

  it("runs an activity workflow end to end", async () => {
    const tq = `ts-sdk-it-${Date.now()}`;
    const acts = { echo: async (s: string) => `echo:${s}` };
    const worker = await Worker.create({
      address: ADDR!,
      namespace: NS,
      taskQueue: tq,
      activities: acts,
      workflows: {
        Echoer: async (s: string) => {
          const a = proxyActivities<typeof acts>({ startToCloseTimeout: "10s" });
          return a.echo(s);
        },
      },
    });
    await worker.start();
    cleanup.push(() => worker.shutdown());

    const client = await TasksClient.connect({ address: ADDR!, namespace: NS });
    cleanup.push(() => client.close());

    const handle = await client.startWorkflow("Echoer", {
      taskQueue: tq,
      workflowId: `it-${Date.now()}`,
      args: ["hi"],
    });

    const deadline = Date.now() + 20000;
    let status = WorkflowStatus.Running;
    while (Date.now() < deadline) {
      const info = await client.describeWorkflow(handle.workflowId, handle.runId);
      status = info.status;
      if (status !== WorkflowStatus.Running) break;
      await new Promise((r) => setTimeout(r, 250));
    }
    expect(status).toBe(WorkflowStatus.Completed);
  });
});
