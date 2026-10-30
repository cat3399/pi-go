import assert from "node:assert/strict";
import test from "node:test";
import { BackNavigation } from "../src/back-navigation.ts";

test("Back dismisses one topmost layer before falling through to the host", () => {
  const navigation = new BackNavigation(), closed = [];
  const settings = navigation.register(() => closed.push("settings"), 30);
  const preview = navigation.register(() => closed.push("preview"), 50);
  const menu = navigation.register(() => closed.push("menu"), 70);
  assert.equal(navigation.handle(), true);
  assert.deepEqual(closed, ["menu"]);
  menu();
  assert.equal(navigation.handle(), true);
  assert.deepEqual(closed, ["menu", "preview"]);
  preview();
  navigation.handle();
  assert.deepEqual(closed, ["menu", "preview", "settings"]);
  settings();
  assert.equal(navigation.handle(), false);
});

test("dialog ordering does not depend on parent effects registering after their children", () => {
  const navigation = new BackNavigation(), closed = [];
  const first = navigation.register(() => closed.push("first dialog"), 40);
  const second = navigation.register(() => closed.push("second dialog"), 40);
  navigation.register(() => closed.push("page"), 30);
  navigation.handle();
  second();
  navigation.handle();
  first();
  navigation.handle();
  assert.deepEqual(closed, ["second dialog", "first dialog", "page"]);
});
