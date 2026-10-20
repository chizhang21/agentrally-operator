// Check that the packaged shell can start an interactive PTY without network access.
const fs = require("node:fs");
const pty = require("/usr/local/lib/node_modules/@brutalstrikedevs/codexapp/node_modules/node-pty");
const shell = process.env.SHELL || "/bin/zsh";
if (!fs.existsSync(shell)) throw new Error("Terminal shell is missing: " + shell);
const terminal = pty.spawn(shell, ["-lc", "printf ARALLY_PTY_OK"], {
  name: "xterm-256color", cols: 80, rows: 24,
  cwd: "/workspace", env: {...process.env}
});
let output = "";
const timeout = setTimeout(() => { terminal.kill(); process.exit(1); }, 5000);
terminal.onData(data => { output += data; });
terminal.onExit(({exitCode}) => {
  clearTimeout(timeout);
  if (exitCode !== 0 || !output.includes("ARALLY_PTY_OK")) process.exit(1);
  console.log("Interactive terminal check passed.");
});
