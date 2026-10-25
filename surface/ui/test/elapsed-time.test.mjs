import assert from "node:assert/strict";
import test from "node:test";
import { ElapsedClock, elapsedBetween } from "../src/workbench/elapsed-time.ts";

test("a new run advances even when the browser clock is behind the server", () => {
  for (const skew of [250, 90_000]) {
    const clock = new ElapsedClock();
    assert.equal(clock.read(100_000, 10, 100_000 - skew), 0);
    assert.equal(clock.read(100_000, 1010, 101_000 - skew), 1000);
    assert.equal(clock.read(100_000, 2010, 102_000 - skew), 2000);
  }
});

test("wall-clock jumps and durable header handoff do not reset a running timer", () => {
  const clock = new ElapsedClock();
  assert.equal(clock.read(100_000, 10, 100_100), 100);
  assert.equal(clock.read(100_005, 1010, 20_000), 1100);
  assert.equal(clock.read(100_005, 2010, 900_000), 2100);
});

test("opening an ongoing run retains its existing elapsed estimate", () => {
  const clock = new ElapsedClock();
  assert.equal(clock.read(100_000, 10, 160_000), 60_000);
  assert.equal(clock.read(100_000, 1010, 161_000), 61_000);
});

test("completed durations use server timestamps and still reject missing or invalid history", () => {
  assert.equal(elapsedBetween(100_000, 101_750), 1750);
  assert.equal(elapsedBetween(null, 101_750), null);
  assert.equal(elapsedBetween(100_000, null), null);
  assert.equal(elapsedBetween(100_000, 99_999), null);
});
