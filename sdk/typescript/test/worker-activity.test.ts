// Copyright © 2026 Hanzo AI. MIT License.
//
// End-to-end durable loop over the real ZAP transport: client starts a
// workflow, the mock server delivers a workflow task, the worker replays and
// schedules an activity, the server delivers the activity task, the worker
// runs it and responds, the server appends the result and re-delivers the
// workflow task, and the workflow completes.

import { describe, it, expect, beforeEach, afterEach } from "vitest";
import { MockTasksServer } from "./mock-server";
import { TasksClient } from "../src/client";
import { Worker } from "../src/worker";
import { proxyActivities, sleep } from "../src/workflow";

async function waitForStatus(server: MockTasksServer, runId: string, status: number, timeoutMs = 8000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const run = server.getRun(runId);
    if (run && run.status === status) return;
    if (Date.now() > deadline) {
      throw new Error(`timeout waiting for status ${status}; got ${run?.status}`);
    }
    await new Promise((r) => setTimeout(r, 20));
  }
}

describe("Worker — full durable activity round-trip", () => {
  let server: MockTasksServer;
  let client: TasksClient;
  let worker: Worker;
  let port: number;

  beforeEach(async () => {
    server = new MockTasksServer();
    port = await server.listen();
    client = await TasksClient.connect({ address: `127.0.0.1:${port}` });
  });

  afterEach(async () => {
    await worker?.shutdown();
    await client.close();
    await server.close();
  });

  it("runs an activity and completes the workflow", async () => {
    const greeting = { greet: async (name: string) => `hello ${name}` };
    worker = await Worker.create({
      address: `127.0.0.1:${port}`,
      taskQueue: "q",
      activities: greeting,
      workflows: {
        Greeter: async (name: string) => {
          const acts = proxyActivities<typeof greeting>({ startToCloseTimeout: "5s" });
          return acts.greet(name);
        },
      },
    });
    await worker.start();

    const handle = await client.startWorkflow("Greeter", { taskQueue: "q", workflowId: "g1", args: ["bob"] });
    await waitForStatus(server, handle.runId, 2 /* Completed */);

    const run = server.getRun(handle.runId)!;
    const completed = run.history.find((e) => e.eventType === "ACTIVITY_TASK_COMPLETED");
    expect(completed?.attributes?.result).toBe("hello bob");
  });

  it("propagates an activity failure to the workflow", async () => {
    worker = await Worker.create({
      address: `127.0.0.1:${port}`,
      taskQueue: "q",
      activities: {
        boom: async () => {
          throw new Error("kaboom");
        },
      },
      workflows: {
        Boomer: async () => {
          const acts = proxyActivities<{ boom(): Promise<void> }>({ startToCloseTimeout: "5s" });
          await acts.boom();
          return "unreached";
        },
      },
    });
    await worker.start();

    const handle = await client.startWorkflow("Boomer", { taskQueue: "q", workflowId: "b1" });
    await waitForStatus(server, handle.runId, 3 /* Failed */);

    const run = server.getRun(handle.runId)!;
    const failed = run.history.find((e) => e.eventType === "ACTIVITY_TASK_FAILED");
    expect(failed).toBeDefined();
  });

  it("fires a durable timer (activity-backed sleep) and completes", async () => {
    worker = await Worker.create({
      address: `127.0.0.1:${port}`,
      taskQueue: "q",
      workflows: {
        Sleeper: async () => {
          await sleep(30);
          return "awake";
        },
      },
    });
    await worker.start();

    const handle = await client.startWorkflow("Sleeper", { taskQueue: "q", workflowId: "s1" });
    await waitForStatus(server, handle.runId, 2 /* Completed */);

    const run = server.getRun(handle.runId)!;
    const timer = run.history.find(
      (e) => e.eventType === "ACTIVITY_TASK_SCHEDULED" && e.attributes?.activityType === "__hanzo_timer__",
    );
    expect(timer).toBeDefined();
  });

  it("chains two activities in program order (seq 0 then seq 1)", async () => {
    const acts = {
      double: async (n: number) => n * 2,
      inc: async (n: number) => n + 1,
    };
    worker = await Worker.create({
      address: `127.0.0.1:${port}`,
      taskQueue: "q",
      activities: acts,
      workflows: {
        Pipe: async (n: number) => {
          const a = proxyActivities<typeof acts>({ startToCloseTimeout: "5s" });
          const d = await a.double(n);
          return a.inc(d);
        },
      },
    });
    await worker.start();

    const handle = await client.startWorkflow("Pipe", { taskQueue: "q", workflowId: "p1", args: [10] });
    await waitForStatus(server, handle.runId, 2 /* Completed */);

    const run = server.getRun(handle.runId)!;
    const results = run.history
      .filter((e) => e.eventType === "ACTIVITY_TASK_COMPLETED")
      .map((e) => e.attributes?.result);
    expect(results).toEqual([20, 21]);
  });
});
