// Copyright © 2026 Hanzo AI. MIT License.

import { describe, it, expect, beforeAll, afterAll } from "vitest";
import { MockTasksServer } from "./mock-server";
import { TasksClient } from "../src/client";
import { WorkflowStatus } from "../src/client/options";

describe("TasksClient — client RPCs over the real ZAP transport", () => {
  let server: MockTasksServer;
  let client: TasksClient;

  beforeAll(async () => {
    server = new MockTasksServer({ queryResult: { count: 7 } });
    const port = await server.listen();
    client = await TasksClient.connect({ address: `127.0.0.1:${port}`, namespace: "default" });
  });

  afterAll(async () => {
    await client.close();
    await server.close();
  });

  it("connects and reports health over the wire", async () => {
    const h = await client.health();
    expect(h.service).toBe("hanzo-tasks");
    expect(h.status).toBe("ok");
  });

  it("starts a workflow and reads it back via describe (memo round-trips)", async () => {
    const handle = await client.startWorkflow("Greeter", {
      taskQueue: "q",
      workflowId: "wf-describe",
      args: ["bob"],
      memo: { source: "test" },
      // search attributes are accepted for API compatibility (ignored on v1 wire)
      searchAttributes: { CustomKeyword: "abc" },
    });
    expect(handle.runId).not.toBe("");

    const info = await client.describeWorkflow("wf-describe", handle.runId);
    expect(info.workflowType).toBe("Greeter");
    expect(info.status).toBe(WorkflowStatus.Running);
    expect(info.taskQueue).toBe("q");
    expect(info.memo).toEqual({ source: "test" });
  });

  it("queries a workflow and decodes the base64 result", async () => {
    const handle = await client.startWorkflow("Greeter", { taskQueue: "q", workflowId: "wf-query" });
    const result = await handle.query<{ count: number }>("getState");
    expect(result).toEqual({ count: 7 });
  });

  it("signals a workflow without error", async () => {
    const handle = await client.startWorkflow("Greeter", { taskQueue: "q", workflowId: "wf-signal" });
    await expect(handle.signal("go", { n: 1 })).resolves.toBeUndefined();
  });

  it("cancels a workflow and observes the terminal status", async () => {
    const handle = await client.startWorkflow("Greeter", { taskQueue: "q", workflowId: "wf-cancel" });
    await handle.cancel();
    const info = await client.describeWorkflow("wf-cancel", handle.runId);
    expect(info.status).toBe(WorkflowStatus.Canceled);
  });

  it("registers a namespace", async () => {
    await expect(client.registerNamespace({ name: "playground", retentionMs: 60000 })).resolves.toBeUndefined();
  });
});
