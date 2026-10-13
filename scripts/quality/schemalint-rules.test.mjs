import assert from 'node:assert/strict';
import test from 'node:test';
import comments from './schemalint/rules/comments.js';
import naming from './schemalint/rules/naming.js';

const run = (rule, schemaObject) => {
  const reports = [];
  rule.process({ schemaObject, report: (report) => reports.push(report) });
  return reports;
};

const emptySchema = (overrides = {}) => ({
  tables: [],
  views: [],
  materializedViews: [],
  enums: [],
  functions: [],
  procedures: [],
  ...overrides,
});

const table = (name, overrides = {}) => ({
  schemaName: 'pin_glide',
  name,
  comment: 'Table comment',
  columns: [],
  checks: [],
  triggers: [],
  ...overrides,
});

test('object naming reports tables, views, enums, routines, checks, triggers and foreign keys without the required prefixes', () => {
  const reports = run(naming, emptySchema({
    tables: [table('identityusers', {
      checks: [{ name: 'valid_state' }],
      triggers: [{ name: 'reset' }],
      columns: [{
        name: 'receiver_id',
        ordinalPosition: 1,
        comment: 'Receiver',
        references: [{ name: 'wrong_fk', tableName: 'delivery_receivers', columnName: 'id' }],
      }],
    })],
    views: [{ schemaName: 'pin_glide', name: 'pairing_pairings', comment: 'View comment' }],
    enums: [{ schemaName: 'pin_glide', name: 'deliverystate', comment: 'Enum comment' }],
    functions: [{ schemaName: 'pin_glide', name: 'claim_delivery', comment: 'Function comment' }],
    procedures: [{ schemaName: 'pin_glide', name: 'expire_sessions', comment: 'Procedure comment' }],
  }));
  assert.deepEqual(reports.map((report) => report.identifier).sort(), [
    'pin_glide.claim_delivery',
    'pin_glide.deliverystate',
    'pin_glide.expire_sessions',
    'pin_glide.identityusers',
    'pin_glide.identityusers.reset',
    'pin_glide.identityusers.valid_state',
    'pin_glide.identityusers.wrong_fk',
    'pin_glide.pairing_pairings',
  ]);
  assert.ok(reports.every((report) => report.rule === 'pin-glide-object-naming'));
});

test('object naming accepts a schema that follows the documented names', () => {
  const reports = run(naming, emptySchema({
    tables: [table('identity_devices', {
      checks: [{ name: 'chk_scope' }],
      triggers: [{ name: 'trg_identity_devices_touch' }],
      columns: [{
        name: 'receiver_id',
        ordinalPosition: 1,
        comment: 'Receiver',
        references: [{ name: 'fk_delivery_receivers_id', tableName: 'delivery_receivers', columnName: 'id' }],
      }],
    })],
    views: [{ schemaName: 'pin_glide', name: 'pairing_vw_summary', comment: 'View comment' }],
    enums: [{ schemaName: 'pin_glide', name: 'delivery_state', comment: 'Enum comment' }],
    functions: [{ schemaName: 'pin_glide', name: 'pairing_fn_expire', comment: 'Function comment' }],
    procedures: [{ schemaName: 'pin_glide', name: 'retention_sp_cleanup', comment: 'Procedure comment' }],
  }));
  assert.deepEqual(reports, []);
});

test('object comments report missing table, column, enum, view and parameter documentation', () => {
  const reports = run(comments, emptySchema({
    tables: [table('identity_devices', {
      comment: '',
      columns: [{ name: 'account_id', comment: '   ' }],
    })],
    views: [{ schemaName: 'pin_glide', name: 'pairing_vw_summary', comment: '' }],
    enums: [{ schemaName: 'pin_glide', name: 'delivery_state', comment: 'Enum comment' }],
    functions: [{
      schemaName: 'pin_glide',
      name: 'pairing_fn_expire',
      comment: 'Expires Pairing Sessions',
      parameters: [{ name: 'p_now' }],
    }],
    procedures: [{
      schemaName: 'pin_glide',
      name: 'retention_sp_cleanup',
      comment: 'Cleans up\nparameters:\np_other: Documented',
      parameters: [{ name: 'p_days' }],
    }],
  }));
  assert.deepEqual(reports.map((report) => report.identifier).sort(), [
    'pin_glide.identity_devices',
    'pin_glide.identity_devices.account_id',
    'pin_glide.pairing_fn_expire',
    'pin_glide.pairing_vw_summary',
    'pin_glide.retention_sp_cleanup',
  ]);
  assert.ok(reports.every((report) => report.rule === 'pin-glide-comments'));
});

test('object comments accept documented objects and routine parameters', () => {
  const reports = run(comments, emptySchema({
    tables: [table('identity_devices', { columns: [{ name: 'account_id', comment: 'Owning Account' }] })],
    views: [{ schemaName: 'pin_glide', name: 'pairing_vw_summary', comment: 'View comment' }],
    enums: [{ schemaName: 'pin_glide', name: 'delivery_state', comment: 'Enum comment' }],
    functions: [{
      schemaName: 'pin_glide',
      name: 'pairing_fn_expire',
      comment: 'Expires Pairing Sessions\nparameters:\np_now: Current time',
      parameters: [{ name: 'p_now' }],
    }],
    procedures: [{
      schemaName: 'pin_glide',
      name: 'retention_sp_cleanup',
      comment: 'Cleans up\nparameters:\np_days: Retention window',
      parameters: [{ name: 'p_days' }],
    }],
  }));
  assert.deepEqual(reports, []);
});
