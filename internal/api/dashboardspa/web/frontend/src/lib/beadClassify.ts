import type { SupervisorBead } from '../supervisor/beadReads';

// Splits the supervisor bead list into operator work and coordination
// records. Workflow steps hang off their source ticket through the
// workflow root's input convoy; convoys collapse onto their members; mail,
// sessions and other runtime bookkeeping are counted but hidden by default.

export type BeadKind = 'work' | 'step' | 'convoy' | 'bookkeeping';

export interface RigPrefixSource {
  name: string;
  prefix?: string;
}

export interface StepSummary {
  id: string;
  /** Short step name, e.g. `deliver` from `mol-gfx-pr-work.deliver`. */
  name: string;
  status: string;
  holds: string[];
}

export interface WorkAnnotation {
  rig: string;
  steps: StepSummary[];
  convoys: string[];
  /** Human-readable source/step disagreements; empty when consistent. */
  mismatches: string[];
}

export interface BeadClassification {
  kindOf: Map<string, BeadKind>;
  rigOf: Map<string, string>;
  /** Work beads and their annotations. */
  work: SupervisorBead[];
  annotations: Map<string, WorkAnnotation>;
  /** Everything hidden by default: bookkeeping, convoys, unlinked steps. */
  hidden: SupervisorBead[];
  /** Hidden counts keyed by display category. */
  hiddenCounts: Map<string, number>;
}

const WORK_TYPES: ReadonlySet<string> = new Set([
  'feature',
  'bug',
  'task',
  'epic',
  'chore',
  'decision',
]);

const STEP_KINDS: ReadonlySet<string> = new Set(['workflow', 'workflow-finalize']);

function meta(bead: SupervisorBead, key: string): string | undefined {
  const value = bead.metadata?.[key];
  return value === undefined || value.length === 0 ? undefined : value;
}

export function beadHolds(bead: SupervisorBead): string[] {
  return (bead.labels ?? []).filter((label) => label.startsWith('hold:'));
}

export function classifyBead(bead: SupervisorBead): BeadKind {
  if (bead.issue_type === 'convoy') return 'convoy';
  const kind = meta(bead, 'gc.kind');
  if (
    (kind !== undefined && STEP_KINDS.has(kind)) ||
    meta(bead, 'gc.step_id') !== undefined ||
    meta(bead, 'gc.root_bead_id') !== undefined
  ) {
    return 'step';
  }
  if (!WORK_TYPES.has(bead.issue_type)) return 'bookkeeping';
  if ((bead.labels ?? []).some((label) => label.startsWith('gc:'))) return 'bookkeeping';
  return 'work';
}

function idPrefix(id: string): string {
  const dash = id.indexOf('-');
  return dash > 0 ? id.slice(0, dash) : id;
}

export function beadRig(bead: SupervisorBead, prefixToRig: ReadonlyMap<string, string>): string {
  const byPrefix = prefixToRig.get(idPrefix(bead.id));
  if (byPrefix !== undefined) return byPrefix;
  const storeRef = meta(bead, 'gc.root_store_ref');
  if (storeRef?.startsWith('rig:')) return storeRef.slice('rig:'.length);
  const rigVar = meta(bead, 'gc.var.rig_name') ?? meta(bead, 'gc.execution_rig_context');
  if (rigVar !== undefined) return rigVar;
  return idPrefix(bead.id);
}

function hiddenCategory(bead: SupervisorBead, kind: BeadKind): string {
  if (kind === 'convoy') return 'convoy';
  if (kind === 'step') return 'unlinked step';
  if ((bead.labels ?? []).includes('gc:nudge')) return 'nudge';
  return bead.issue_type;
}

function stepName(bead: SupervisorBead): string {
  const stepId = meta(bead, 'gc.step_id') ?? bead.title;
  const dot = stepId.lastIndexOf('.');
  return dot >= 0 ? stepId.slice(dot + 1) : stepId;
}

function stepMismatches(source: SupervisorBead, steps: readonly StepSummary[]): string[] {
  const out: string[] = [];
  if (source.status === 'closed') {
    const live = steps.filter((step) => step.status !== 'closed');
    if (live.length > 0) out.push(`source closed, ${live.length} step(s) still open`);
    return out;
  }
  const sourceHolds = new Set(beadHolds(source));
  for (const step of steps) {
    if (step.status === 'closed') continue;
    if (step.status === 'blocked' && source.status !== 'blocked') {
      out.push(`step ${step.name} blocked, source ${source.status.replace('_', ' ')}`);
    } else if (source.status === 'blocked' && step.status !== 'blocked') {
      out.push(`source blocked, step ${step.name} ${step.status.replace('_', ' ')}`);
    }
    const missing = step.holds.filter((hold) => !sourceHolds.has(hold));
    if (missing.length > 0)
      out.push(`step ${step.name} has ${missing.join(', ')}, source does not`);
  }
  return out;
}

export function classifyBeads(
  beads: readonly SupervisorBead[],
  rigs: readonly RigPrefixSource[],
): BeadClassification {
  const prefixToRig = new Map<string, string>();
  for (const rig of rigs) {
    if (rig.prefix) prefixToRig.set(rig.prefix, rig.name);
  }
  const byId = new Map<string, SupervisorBead>();
  const kindOf = new Map<string, BeadKind>();
  const rigOf = new Map<string, string>();
  for (const bead of beads) {
    byId.set(bead.id, bead);
    kindOf.set(bead.id, classifyBead(bead));
    rigOf.set(bead.id, beadRig(bead, prefixToRig));
  }

  // Convoy -> tracked members.
  const convoyMembers = new Map<string, string[]>();
  for (const bead of beads) {
    if (kindOf.get(bead.id) !== 'convoy') continue;
    const members = (bead.dependencies ?? [])
      .filter((dep) => dep.type === 'tracks' && dep.depends_on_id !== bead.id)
      .map((dep) => dep.depends_on_id);
    convoyMembers.set(bead.id, members);
  }

  const annotations = new Map<string, WorkAnnotation>();
  const annotationFor = (id: string): WorkAnnotation => {
    let entry = annotations.get(id);
    if (entry === undefined) {
      entry = { rig: rigOf.get(id) ?? idPrefix(id), steps: [], convoys: [], mismatches: [] };
      annotations.set(id, entry);
    }
    return entry;
  };
  const isWork = (id: string) => kindOf.get(id) === 'work';

  for (const [convoyId, members] of convoyMembers) {
    for (const member of members) {
      if (isWork(member)) annotationFor(member).convoys.push(convoyId);
    }
  }

  // Step -> source: step's workflow root names its input convoy, whose
  // tracked work members are the source tickets.
  const linkedSteps = new Set<string>();
  for (const bead of beads) {
    if (kindOf.get(bead.id) !== 'step') continue;
    const rootId = meta(bead, 'gc.root_bead_id') ?? bead.id;
    const root = byId.get(rootId) ?? bead;
    const convoyId = meta(root, 'gc.input_convoy_id') ?? meta(root, 'gc.var.convoy_id');
    if (convoyId === undefined) continue;
    const sources = (convoyMembers.get(convoyId) ?? []).filter(isWork);
    if (sources.length === 0) continue;
    linkedSteps.add(bead.id);
    // The workflow root and finalize records mirror the step; keep them out of the summary.
    const kind = meta(bead, 'gc.kind');
    if (kind !== undefined && STEP_KINDS.has(kind)) continue;
    for (const source of sources) {
      annotationFor(source).steps.push({
        id: bead.id,
        name: stepName(bead),
        status: bead.status,
        holds: beadHolds(bead),
      });
    }
  }

  const work: SupervisorBead[] = [];
  const hidden: SupervisorBead[] = [];
  const hiddenCounts = new Map<string, number>();
  for (const bead of beads) {
    const kind = kindOf.get(bead.id);
    if (kind === 'work') {
      work.push(bead);
      const entry = annotationFor(bead.id);
      entry.steps.sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
      entry.mismatches = stepMismatches(bead, entry.steps);
      continue;
    }
    if (kind === 'step' && linkedSteps.has(bead.id)) continue;
    if (kind === 'convoy' && (convoyMembers.get(bead.id) ?? []).some(isWork)) continue;
    hidden.push(bead);
    const category = hiddenCategory(bead, kind ?? 'bookkeeping');
    hiddenCounts.set(category, (hiddenCounts.get(category) ?? 0) + 1);
  }

  return { kindOf, rigOf, work, annotations, hidden, hiddenCounts };
}
