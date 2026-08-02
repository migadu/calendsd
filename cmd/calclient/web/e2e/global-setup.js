// Builds calserver + calclient and starts them against the shared dev Postgres,
// so the specs run end-to-end against the real server. PIDs are written to
// .pids for global-teardown. Requires `scripts/dev-db.sh start` beforehand.
const { execFileSync, spawn } = require('child_process');
const fs = require('fs');
const path = require('path');
const http = require('http');

const ROOT = path.resolve(__dirname, '../../../..');        // services/calserver
const BIN = path.join(__dirname, '.bin');
const PIDS = path.join(__dirname, '.pids');
const DSN = `postgres://${process.env.USER}@/calserver?host=${ROOT}/.dev/pg`;

function waitFor(url, timeoutMs = 20_000) {
  const deadline = Date.now() + timeoutMs;
  return new Promise((resolve, reject) => {
    const tick = () => {
      const req = http.get(url, res => { res.resume(); resolve(); }); // any response = up
      req.on('error', () => {
        if (Date.now() > deadline) reject(new Error(`timed out waiting for ${url}`));
        else setTimeout(tick, 300);
      });
      req.setTimeout(1000, () => req.destroy());
    };
    tick();
  });
}

module.exports = async () => {
  fs.mkdirSync(BIN, { recursive: true });
  try {
    execFileSync('go', ['build', '-o', path.join(BIN, 'calserver'), './cmd/calserver'], { cwd: ROOT, stdio: 'inherit' });
    execFileSync('go', ['build', '-o', path.join(BIN, 'calclient'), './cmd/calclient'], { cwd: ROOT, stdio: 'inherit' });
  } catch (e) {
    throw new Error('go build failed — is the Go toolchain available?\n' + e.message);
  }

  const outLog = fs.openSync(path.join(__dirname, '.server.log'), 'w');
  const srv = spawn(path.join(BIN, 'calserver'),
    ['-addr', '127.0.0.1:8080', '-db', DSN],
    { stdio: ['ignore', outLog, outLog], detached: true });
  const cli = spawn(path.join(BIN, 'calclient'),
    ['-addr', '127.0.0.1:8091', '-target', 'http://127.0.0.1:8080', '-dev-assets', path.join(ROOT, 'cmd/calclient/web')],
    { stdio: ['ignore', outLog, outLog], detached: true });
  srv.unref(); cli.unref();
  fs.writeFileSync(PIDS, JSON.stringify([srv.pid, cli.pid]));

  try {
    await waitFor('http://127.0.0.1:8080/');    // 401 counts as a response
    await waitFor('http://127.0.0.1:8091/cal/');
  } catch (e) {
    throw new Error(`${e.message}\nIs the dev DB running? Start it with: scripts/dev-db.sh start`);
  }
};
