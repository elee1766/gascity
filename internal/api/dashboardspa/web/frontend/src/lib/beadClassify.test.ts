import { describe, expect, it } from 'vitest';
import type { SupervisorBead } from '../supervisor/beadReads';
import { classifyBeads } from './beadClassify';

// Record shapes copied from a live gfx-oku /v0/city/<city>/beads response:
// the rig field is absent, workflow steps live in the city store (gcg-*),
// and the input convoy tracks the source ticket.
const RIGS = [
  { name: 'gascity', prefix: 'gci' },
  { name: 'trade', prefix: 'trd' },
];

function bead(id: string, extra: Partial<SupervisorBead> = {}): SupervisorBead {
  return {
    id,
    title: id,
    status: 'open',
    issue_type: 'task',
    created_at: '2026-10-27T01:40:54Z',
    ...extra,
  };
}

const source = bead('gci-267265', { title: 'Dashboard beads page' });
const convoy = bead('gci-269313', {
  issue_type: 'convoy',
  metadata: { 'gc.synthetic': 'true' },
  dependencies: [{ issue_id: 'gci-269313', depends_on_id: 'gci-267265', type: 'tracks' }],
});
const root = bead('gcg-1448961', {
  status: 'in_progress',
  metadata: {
    'gc.kind': 'workflow',
    'gc.input_convoy_id': 'gci-269313',
    'gc.root_store_ref': 'rig:gascity',
    'gc.step_id': 'mol-gfx-pr-work',
  },
});
const deliver = bead('gcg-1448962', {
  status: 'blocked',
  labels: ['hold:external'],
  metadata: {
    'gc.root_bead_id': 'gcg-1448961',
    'gc.root_store_ref': 'rig:gascity',
    'gc.step_id': 'mol-gfx-pr-work.deliver',
  },
});
const finalize = bead('gcg-1448963', {
  metadata: {
    'gc.kind': 'workflow-finalize',
    'gc.root_bead_id': 'gcg-1448961',
    'gc.root_store_ref': 'rig:gascity',
    'gc.step_id': 'mol-gfx-pr-work.workflow-finalize',
  },
});
const mail = bead('gcg-1400000', { issue_type: 'message' });
const nudge = bead('gcg-1447937', { issue_type: 'chore', labels: ['gc:nudge'] });
const orphanStep = bead('gcg-1300001', {
  metadata: { 'gc.root_bead_id': 'gcg-missing', 'gc.root_store_ref': 'rig:trade' },
});

describe('classifyBeads', () => {
  const result = classifyBeads(
    [source, convoy, root, deliver, finalize, mail, nudge, orphanStep],
    RIGS,
  );

  it('keeps only work tickets visible and counts the rest by category', () => {
    expect(result.work.map((b) => b.id)).toEqual(['gci-267265']);
    expect(Object.fromEntries(result.hiddenCounts)).toEqual({
      message: 1,
      nudge: 1,
      'unlinked step': 1,
    });
  });

  it('groups the workflow step and convoy under the source ticket', () => {
    const annotation = result.annotations.get('gci-267265');
    expect(annotation?.steps).toEqual([
      { id: 'gcg-1448962', name: 'deliver', status: 'blocked', holds: ['hold:external'] },
    ]);
    expect(annotation?.convoys).toEqual(['gci-269313']);
  });

  it('flags a blocked, held step under an open source', () => {
    expect(result.annotations.get('gci-267265')?.mismatches).toEqual([
      'step deliver blocked, source open',
      'step deliver has hold:external, source does not',
    ]);
  });

  it('derives the rig from the id prefix, then the store binding', () => {
    expect(result.rigOf.get('gci-267265')).toBe('gascity');
    expect(result.rigOf.get('gcg-1448962')).toBe('gascity');
    expect(result.rigOf.get('gcg-1300001')).toBe('trade');
  });

  it('reports no mismatch when source and step agree', () => {
    const agreed = classifyBeads(
      [{ ...source, status: 'blocked', labels: ['hold:external'] }, convoy, root, deliver],
      RIGS,
    );
    expect(agreed.annotations.get('gci-267265')?.mismatches).toEqual([]);
  });
});
