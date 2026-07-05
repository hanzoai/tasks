// Copyright © 2026 Hanzo AI. MIT License.
//
// Proves the exact property social's cutover depends on: the TS worker/client
// run durable work against an IDENTITY-GATED tasksd (RequireIdentity), auth'd by
// an IAM bearer that rides every JSON RPC as auth_token. Boots a real tasksd
// with a local RS256 JWKS. Skipped unless TASKSD_BIN points at a built binary:
//
//   go build -o /tmp/tasksd ./cmd/tasksd
//   TASKSD_BIN=/tmp/tasksd npx vitest run test/gated.integration.test.ts

import { describe, it, expect, beforeAll, afterAll } from "vitest";
import net from "node:net";
import http from "node:http";
import crypto from "node:crypto";
import { spawn, type ChildProcess } from "node:child_process";
import os from "node:os";
import fs from "node:fs";
import path from "node:path";
import { TasksClient } from "../src/client";
import { Worker } from "../src/worker";
import { proxyActivities } from "../src/workflow";
import { WorkflowStatus } from "../src/client/options";

const BIN = process.env.TASKSD_BIN;
const suite = BIN ? describe : describe.skip;

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.listen(0, "127.0.0.1", () => {
      const p = (srv.address() as net.AddressInfo).port;
      srv.close(() => resolve(p));
    });
    srv.on("error", reject);
  });
}

function waitTcp(port: number, ms: number): Promise<void> {
  const deadline = Date.now() + ms;
  return new Promise((resolve, reject) => {
    const tick = () => {
      const s = net.connect(port, "127.0.0.1");
      s.on("connect", () => {
        s.destroy();
        resolve();
      });
      s.on("error", () => {
        s.destroy();
        if (Date.now() > deadline) reject(new Error("tasksd did not start"));
        else setTimeout(tick, 150);
      });
    };
    tick();
  });
}

const b64url = (b: Buffer | string): string => Buffer.from(b).toString("base64url");

suite("gated tasksd — IAM bearer over ZAP", () => {
  const issuer = "https://hanzo.id";
  const kid = "e2e-key-1";
  const owner = "acme-ts-e2e";
  let proc: ChildProcess;
  let jwks: http.Server;
  let zapPort = 0;
  let token = "";

  beforeAll(async () => {
    const { publicKey, privateKey } = crypto.generateKeyPairSync("rsa", { modulusLength: 2048 });
    const jwk = { ...(publicKey.export({ format: "jwk" }) as object), kid, alg: "RS256", use: "sig" };

    const jwksPort = await freePort();
    jwks = http.createServer((_req, res) => {
      res.setHeader("content-type", "application/json");
      res.end(JSON.stringify({ keys: [jwk] }));
    });
    await new Promise<void>((r) => jwks.listen(jwksPort, "127.0.0.1", r));

    // Mint an owner-claimed RS256 JWT — the shape hanzo.id issues.
    const header = b64url(JSON.stringify({ alg: "RS256", typ: "JWT", kid }));
    const payload = b64url(
      JSON.stringify({ iss: issuer, sub: "u1", owner, exp: Math.floor(Date.now() / 1000) + 3600 }),
    );
    const signingInput = `${header}.${payload}`;
    const sig = crypto.sign("RSA-SHA256", Buffer.from(signingInput), privateKey);
    token = `${signingInput}.${b64url(sig)}`;

    zapPort = await freePort();
    const httpPort = await freePort();
    const dataDir = fs.mkdtempSync(path.join(os.tmpdir(), "tasksd-gated-"));
    proc = spawn(BIN!, ["--zap-port", String(zapPort), "--http", `:${httpPort}`, "--data", dataDir], {
      env: {
        ...process.env,
        TASKSD_REQUIRE_IDENTITY: "true",
        TASKSD_JWKS_URL: `http://127.0.0.1:${jwksPort}`,
        TASKSD_JWT_ISSUER: issuer,
      },
      stdio: "ignore",
    });
    await waitTcp(zapPort, 15000);
  }, 30000);

  afterAll(async () => {
    proc?.kill("SIGKILL");
    await new Promise<void>((r) => jwks.close(() => r()));
  });

  it("rejects an anonymous client", async () => {
    const client = await TasksClient.connect({ address: `127.0.0.1:${zapPort}`, namespace: "default" });
    await expect(
      client.startWorkflow("Denied", { taskQueue: "q", workflowId: "denied", args: [] }),
    ).rejects.toThrow(/401|auth_token/i);
    await client.close();
  });

  it("runs a durable workflow authenticated by the IAM bearer", async () => {
    const acts = { greet: async (who: string) => `hi, ${who}` };
    const tokenSource = () => token;

    const client = await TasksClient.connect({
      address: `127.0.0.1:${zapPort}`,
      namespace: "default",
      token: tokenSource,
    });
    // WithOrg(owner) prefixes the store; the org's `default` namespace must be
    // registered before ExecuteWorkflow (the embedded engine never lazily creates it).
    await client.registerNamespace({ name: "default" });

    const worker = await Worker.create({
      address: `127.0.0.1:${zapPort}`,
      namespace: "default",
      taskQueue: "gated-q",
      token: tokenSource,
      activities: acts,
      workflows: {
        Greeter: async (who: string) => {
          const a = proxyActivities<typeof acts>({ startToCloseTimeout: "10s" });
          return a.greet(who);
        },
      },
    });
    await worker.start();

    const handle = await client.startWorkflow("Greeter", {
      taskQueue: "gated-q",
      workflowId: `gated-${Date.now()}`,
      args: ["hanzo"],
    });

    const deadline = Date.now() + 20000;
    let status = WorkflowStatus.Running;
    while (Date.now() < deadline) {
      status = (await client.describeWorkflow(handle.workflowId, handle.runId)).status;
      if (status !== WorkflowStatus.Running) break;
      await new Promise((r) => setTimeout(r, 200));
    }
    expect(status).toBe(WorkflowStatus.Completed);
    await worker.shutdown();
    await client.close();
  }, 30000);
});
