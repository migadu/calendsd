// layout.js — the grid's pure placement math, kept free of DOM and store so
// it can be unit-tested under node (hence the relative import, which resolves
// both in the browser and on the filesystem).

import { DAY, startOfDay, addDays, spansDays } from '../../shared/time.js';

// layoutColumns splits a day's timed events into side-by-side columns so that
// overlapping events sit next to each other instead of stacking. Each event is
// tagged with {col, cols}: its column index and the width of its overlap
// cluster (a maximal run of transitively-overlapping events).
export function layoutColumns(evs) {
  const sorted = evs.slice().sort((a, b) => a.start - b.start || b.end - a.end);
  const out = [];
  let cluster = [], clusterEnd = -Infinity;
  const flush = () => {
    const cols = cluster.reduce((m, x) => Math.max(m, x.col), 0) + 1;
    for (const x of cluster) { x.cols = cols; out.push(x); }
    cluster = [];
  };
  for (const inst of sorted) {
    const s = +inst.start, e = +inst.end;
    if (cluster.length && s >= clusterEnd) flush();   // no overlap with the run so far
    const used = new Set(cluster.filter(x => +x.inst.end > s).map(x => x.col));
    let col = 0; while (used.has(col)) col++;          // leftmost free column
    cluster.push({ inst, col });
    clusterEnd = Math.max(clusterEnd, e);
  }
  flush();
  return out;
}

// spans returns the multi-day events overlapping a week [weekStart, +7d),
// each assigned a lane (greedy) and the columns it covers within the week.
export function spans(instances, weekStart) {
  const weekEnd = addDays(weekStart, 7);
  const col = d => Math.min(6, Math.max(0, Math.round((startOfDay(d) - weekStart) / DAY)));
  const multi = instances
    .filter(i => spansDays(i) && +i.start < +weekEnd && +i.end > +weekStart
      && (col(new Date(+i.end - 1)) > col(i.start) || +i.start < +weekStart))
    .map(i => ({
      inst: i,
      colStart: col(i.start < weekStart ? weekStart : i.start),
      colEnd: col(new Date(Math.min(+i.end - 1, +weekEnd - 1))),
      contLeft: +i.start < +weekStart,
      contRight: +i.end > +weekEnd,
    }))
    .sort((a, b) => a.colStart - b.colStart || (b.colEnd - b.colStart) - (a.colEnd - a.colStart));
  const laneEnds = []; // last covered column per lane
  for (const s of multi) {
    let lane = laneEnds.findIndex(end => end < s.colStart);
    if (lane === -1) { lane = laneEnds.length; laneEnds.push(-1); }
    laneEnds[lane] = s.colEnd;
    s.lane = lane;
  }
  return { multi, laneCount: laneEnds.length };
}
