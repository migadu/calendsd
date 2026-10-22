// Stops the calendsd + calclient processes global-setup started.
const fs = require('fs');
const path = require('path');

const PIDS = path.join(__dirname, '.pids');

module.exports = async () => {
  let pids = [];
  try { pids = JSON.parse(fs.readFileSync(PIDS, 'utf8')); } catch { /* nothing to stop */ }
  for (const pid of pids) {
    try { process.kill(pid); } catch { /* already gone */ }
  }
  try { fs.unlinkSync(PIDS); } catch { /* ignore */ }
};
