// Copyright © 2026 Hanzo AI. MIT License.

import { describe, it, expect } from "vitest";
import { runWorkflowEpisode } from "../src/worker";
import {
  proxyActivities,
  sleep,
  condition,
  defineSignal,
  setHandler,
} from "../src/workflow";
import { CommandKind } from "../src/worker";
import { TIMER_ACTIVITY_TYPE } from "../src/workflow/runtime";
import type { WorkflowInfo } from "../src/workflow";

const info: WorkflowInfo = {
  workflowId: "w1",
  runId: "r1",
  workflowType: "T",
  taskQueue: "q",
  namespace: "default",
  attempt: 1,
};

function started(input: unknown[]) {
  return [{ eventId: 1, eventType: "WORKFLOW_EXECUTION_STARTED", attributes: { input } }];
}

function b64(s: string): string {
  return Buffer.from(s, "utf8").toString("base64");
}

describe("workflow decider — event-sourced replay produces exact wire commands", () => {
  it("emits a ScheduleActivity command when the activity is not yet in history", async () => {
    const wf = async (n: number) => {
      const acts = proxyActivities<{ stepA(n: number): Promise<number> }>({ startToCloseTimeout: 1000 });
      const a = await acts.stepA(n);
      return a + 1;
    };
    const cmds = await runWorkflowEpisode(wf, info, JSON.stringify(started([5])));
    expect(cmds).toHaveLength(1);
    expect(cmds[0].kind).toBe(CommandKind.ScheduleActivity);
    expect(cmds[0].seq).toBe(0);
    expect(cmds[0].activityType).toBe("stepA");
    expect(Buffer.from(cmds[0].input!, "base64").toString("utf8")).toBe("[5]");
  });

  it("completes the workflow once the activity result is in history", async () => {
    const wf = async (n: number) => {
      const acts = proxyActivities<{ stepA(n: number): Promise<number> }>({ startToCloseTimeout: 1000 });
      const a = await acts.stepA(n);
      return a + 1;
    };
    const history = [
      ...started([5]),
      { eventId: 2, eventType: "ACTIVITY_TASK_SCHEDULED", attributes: { seq: 0 } },
      { eventId: 3, eventType: "ACTIVITY_TASK_COMPLETED", attributes: { seq: 0, result: 10 } },
    ];
    const cmds = await runWorkflowEpisode(wf, info, JSON.stringify(history));
    expect(cmds).toHaveLength(1);
    expect(cmds[0].kind).toBe(CommandKind.CompleteWorkflow);
    expect(Buffer.from(cmds[0].result!, "base64").toString("utf8")).toBe("11");
  });

  it("does not re-schedule an activity already scheduled in history", async () => {
    const wf = async () => {
      const acts = proxyActivities<{ stepA(): Promise<number> }>({ startToCloseTimeout: 1000 });
      await acts.stepA();
      return "done";
    };
    const history = [
      ...started([]),
      { eventId: 2, eventType: "ACTIVITY_TASK_SCHEDULED", attributes: { seq: 0 } },
    ];
    const cmds = await runWorkflowEpisode(wf, info, JSON.stringify(history));
    // scheduled but not yet complete → parked, no new commands
    expect(cmds).toHaveLength(0);
  });

  it("propagates an activity failure into a workflow failure", async () => {
    const wf = async () => {
      const acts = proxyActivities<{ boom(): Promise<void> }>({ startToCloseTimeout: 1000 });
      await acts.boom();
      return "unreached";
    };
    const history = [
      ...started([]),
      { eventId: 2, eventType: "ACTIVITY_TASK_SCHEDULED", attributes: { seq: 0 } },
      {
        eventId: 3,
        eventType: "ACTIVITY_TASK_FAILED",
        attributes: { seq: 0, failure: { v: 1, p: { message: "kaboom", code: "ApplicationError", nonRetryable: true } } },
      },
    ];
    const cmds = await runWorkflowEpisode(wf, info, JSON.stringify(history));
    expect(cmds).toHaveLength(1);
    expect(cmds[0].kind).toBe(CommandKind.FailWorkflow);
    const failure = JSON.parse(Buffer.from(cmds[0].failure!, "base64").toString("utf8"));
    expect(failure.p.message).toBe("kaboom");
  });

  it("schedules a durable timer via the internal sleeper activity", async () => {
    const wf = async () => {
      await sleep(1000);
      return "woke";
    };
    const cmds = await runWorkflowEpisode(wf, info, JSON.stringify(started([])));
    expect(cmds).toHaveLength(1);
    expect(cmds[0].kind).toBe(CommandKind.ScheduleActivity);
    expect(cmds[0].activityType).toBe(TIMER_ACTIVITY_TYPE);
    expect(Buffer.from(cmds[0].input!, "base64").toString("utf8")).toBe("[1000]");
  });

  it("parks on condition until a signal flips the predicate", async () => {
    const goSignal = defineSignal("go");
    const wf = async () => {
      let done = false;
      setHandler(goSignal, () => {
        done = true;
      });
      await condition(() => done);
      return "ok";
    };

    // Episode 1: no signal → parked, no commands.
    const first = await runWorkflowEpisode(wf, info, JSON.stringify(started([])));
    expect(first).toHaveLength(0);

    // Episode 2: signal delivered → condition resolves → workflow completes.
    const withSignal = [
      ...started([]),
      { eventId: 2, eventType: "WORKFLOW_EXECUTION_SIGNALED", attributes: { signal: "go", input: undefined } },
    ];
    const second = await runWorkflowEpisode(wf, info, JSON.stringify(withSignal));
    expect(second).toHaveLength(1);
    expect(second[0].kind).toBe(CommandKind.CompleteWorkflow);
  });

  it("survives user try/catch around a parked activity (block escapes catch)", async () => {
    const wf = async () => {
      const acts = proxyActivities<{ stepA(): Promise<number> }>({ startToCloseTimeout: 1000 });
      try {
        await acts.stepA();
      } catch {
        return "swallowed"; // must NOT run on a block
      }
      return "reached";
    };
    const cmds = await runWorkflowEpisode(wf, info, JSON.stringify(started([])));
    // Parked on the activity — no completion command leaked through the catch.
    expect(cmds.some((c) => c.kind === CommandKind.ScheduleActivity)).toBe(true);
    expect(cmds.some((c) => c.kind === CommandKind.CompleteWorkflow)).toBe(false);
  });
});
