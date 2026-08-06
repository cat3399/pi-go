import { spawn, spawnSync } from "node:child_process";

// The original helper selects cross-spawn only on Windows. Compatibility
// golden generation runs on the current Unix workspace, so this module merely
// satisfies its eager import with the same Node primitives used by the live
// branch. No SessionManager scenario invokes process spawning.
spawn.sync = spawnSync;

export default spawn;
