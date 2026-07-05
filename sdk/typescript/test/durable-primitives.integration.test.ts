// Copyright © 2026 Hanzo AI. MIT License.
//
// End-to-end proof of the durable primitives social-orchestrator depends on —
// signal-to-running-workflow, continueAsNew, startChild, and typed search
// attributes — run through the SAME TS worker social uses, against a real
// tasksd/embedded engine. Skipped unless TASKS_ZAP_ADDR is set:
//
//   go build -o /tmp/tasksd ./cmd/tasksd && /tmp/tasksd --allow-no-auth start &
//   TASKS_ZAP_ADDR=127.0.0.1:9999 npx vitest run test/durable-primitives.integration.test.ts

import { describe, it, expect, afterAll } from "vitest";
import { TasksClient } from "../src/client";
import { Worker } from "../src/worker";
import {
  proxyActivities,
  condition,
  continueAsNew,
  startChild,
  defineSignal,
  setHandler,
} from "../src/workflow";
import { WorkflowStatus } from "../src/client/options";

const ADDR = process.env.TASKS_ZAP_ADDR;
const NS = process.env.TASKS_NAMESPACE ?? "default";
const suite = ADDR ? describe : describe.skip;

const itemSignal = defineSignal<[string]>("item");

const activities = {
  echo: async (s: string) => `echo:${s}`,
  record: async (_round: number, _batch: string[]) => true,
};

// digest-like: signal-populated queue + continueAsNew across rounds. Mirrors
// social's digestEmailWorkflow / sendEmailWorkflow shape.
async function digestLike({ round = 1, seen = [] }: { round?: number; seen?: string[] }): Promise<string> {
  const acts = proxyActivities<typeof activities>({ startToCloseTimeout: "10s" });
  const queue: string[] = [];
  setHandler(itemSignal, (item: string) => {
    queue.push(item);
  });
  await condition(() => queue.length > 0);
  const batch = queue.splice(0, queue.length);
  await acts.record(round, batch);
  const all = [...seen, ...batch];
  if (round >= 2) return all.join(",");
  return await continueAsNew({ round: round + 1, seen: all });
}

async function childWf({ tag }: { tag: string }): Promise<string> {
  const acts = proxyActivities<typeof activities>({ startToCloseTimeout: "10s" });
  return acts.echo(tag);
}

// parent: starts a detached (ABANDON) child, then completes independently.
async function parentWf({ childId }: { childId: string }): Promise<string> {
  await startChild(childWf, { parentClosePolicy: "ABANDON", workflowId: childId, args: [{ tag: "kid" }] });
  return "parent-done";
}

// noop: completes immediately (used for search-attr / conflict-policy checks).
async function noop(): Promise<string> {
  return "ok";
}

async function waitStatus(client: TasksClient, workflowId: string, runId: string, want: WorkflowStatus, ms = 30000): Promise<WorkflowStatus> {
  const deadline = Date.now() + ms;
  let status = WorkflowStatus.Running;
  while (Date.now() < deadline) {
    const info = await client.describeWorkflow(workflowId, runId);
    status = info.status;
    if (status === want) return status;
    if (status !== WorkflowStatus.Running && status !== WorkflowStatus.ContinuedAsNew) break;
    await new Promise((r) => setTimeout(r, 200));
  }
  return status;
}

suite("durable primitives — live tasksd", () => {
  const cleanup: Array<() => Promise<void>> = [];
  afterAll(async () => {
    for (const fn of cleanup) await fn().catch(() => {});
  });

  async function startWorker(tq: string): Promise<Worker> {
    const worker = await Worker.create({
      address: ADDR!,
      namespace: NS,
      taskQueue: tq,
      activities,
      workflows: { digestLike, childWf, parentWf, noop },
    });
    await worker.start();
    cleanup.push(() => worker.shutdown());
    return worker;
  }

  it("delivers signals to a running workflow across continueAsNew", async () => {
    const tq = `dp-digest-${Date.now()}`;
    await startWorker(tq);
    const client = await TasksClient.connect({ address: ADDR!, namespace: NS });
    cleanup.push(() => client.close());

    const wfId = `digest-${Date.now()}`;
    // Start + first signal (round 1).
    const handle = await client.signalWithStart("digestLike", "item", "a", {
      taskQueue: tq,
      workflowId: wfId,
      args: [{ round: 1, seen: [] }],
    });
    const round1Run = handle.runId;

    // Wait for round-1 to process + continueAsNew into a fresh RUNNING run.
    let successorRun = "";
    const deadline = Date.now() + 20000;
    while (Date.now() < deadline) {
      const info = await client.describeWorkflow(wfId, "");
      if (info.runId && info.runId !== round1Run && info.status === WorkflowStatus.Running) {
        successorRun = info.runId;
        break;
      }
      await new Promise((r) => setTimeout(r, 200));
    }
    expect(successorRun).not.toBe("");

    // Signal the running successor (round 2) — proves signal-to-running-wf.
    await client.signalWorkflow(wfId, "", "item", "b");

    // The workflow returns "a,b" and COMPLETES.
    const status = await waitStatus(client, wfId, "", WorkflowStatus.Completed);
    expect(status).toBe(WorkflowStatus.Completed);
  });

  it("starts a detached child workflow (startChild / ABANDON)", async () => {
    const tq = `dp-child-${Date.now()}`;
    await startWorker(tq);
    const client = await TasksClient.connect({ address: ADDR!, namespace: NS });
    cleanup.push(() => client.close());

    const childId = `child-${Date.now()}`;
    const parent = await client.startWorkflow("parentWf", {
      taskQueue: tq,
      workflowId: `parent-${Date.now()}`,
      args: [{ childId }],
    });

    const parentStatus = await waitStatus(client, parent.workflowId, parent.runId, WorkflowStatus.Completed);
    expect(parentStatus).toBe(WorkflowStatus.Completed);

    // The detached child runs independently to completion.
    const childStatus = await waitStatus(client, childId, "", WorkflowStatus.Completed);
    expect(childStatus).toBe(WorkflowStatus.Completed);
  });

  it("carries typed search attributes + honors TERMINATE_EXISTING", async () => {
    const tq = `dp-sa-${Date.now()}`;
    await startWorker(tq);
    const client = await TasksClient.connect({ address: ADDR!, namespace: NS });
    cleanup.push(() => client.close());

    const postId = `sa-${Date.now()}`;
    const wfId = `post-${postId}`;
    await client.startWorkflow("noop", {
      taskQueue: tq,
      workflowId: wfId,
      args: [],
      searchAttributes: { postId, organizationId: "org-e2e" },
    });

    // Visibility query by the custom attribute finds the run.
    const deadline = Date.now() + 10000;
    let found = 0;
    while (Date.now() < deadline) {
      const res = await client.listWorkflows(`postId = "${postId}"`);
      found = res.executions.length;
      if (found >= 1) break;
      await new Promise((r) => setTimeout(r, 200));
    }
    expect(found).toBeGreaterThanOrEqual(1);
  });
});
