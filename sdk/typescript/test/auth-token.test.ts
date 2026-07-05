// Copyright © 2026 Hanzo AI. MIT License.
//
// The IAM bearer must ride every JSON-body RPC as `auth_token` so an
// identity-gated frontend (cloud ServeGated :9999) can scope the request to
// the token owner's org. Field-object frames (worker respond) are HMAC-
// authenticated and must NOT carry it.

import { describe, it, expect, afterEach } from "vitest";
import { MockTasksServer } from "./mock-server";
import { TasksClient } from "../src/client";

describe("auth_token injection", () => {
  let server: MockTasksServer;
  afterEach(async () => {
    await server?.close();
  });

  it("rides client RPCs when a token source is configured", async () => {
    server = new MockTasksServer();
    const port = await server.listen();

    let calls = 0;
    const client = await TasksClient.connect({
      address: `127.0.0.1:${port}`,
      namespace: "default",
      token: () => {
        calls++;
        return "iam-bearer-xyz";
      },
    });

    await client.startWorkflow("Demo", { taskQueue: "q", workflowId: "wf-auth", args: [] });
    await client.signalWorkflow("wf-auth", "", "poke", { n: 1 });
    await client.close();

    expect(calls).toBeGreaterThanOrEqual(2);
    expect(server.seenAuthTokens.length).toBeGreaterThanOrEqual(2);
    expect(server.seenAuthTokens.every((t) => t === "iam-bearer-xyz")).toBe(true);
  });

  it("omits auth_token when no token source is set", async () => {
    server = new MockTasksServer();
    const port = await server.listen();
    const client = await TasksClient.connect({ address: `127.0.0.1:${port}`, namespace: "default" });
    await client.startWorkflow("Demo", { taskQueue: "q", workflowId: "wf-noauth", args: [] });
    await client.close();
    expect(server.seenAuthTokens.every((t) => t === undefined)).toBe(true);
  });
});
