const identifier = (object) => `${object.schemaName}.${object.name}`;
const reportName = (report, object, rule, pattern) => {
  if (!pattern.test(object.name)) {
    report({
      rule,
      identifier: identifier(object),
      message: `name must match ${pattern}`,
    });
  }
};

const naming = {
  name: 'pin-glide-object-naming',
  docs: { description: 'Require Pin Glide object naming conventions.' },
  process({ schemaObject, report }) {
    for (const table of schemaObject.tables) {
      reportName(report, table, naming.name, /^[a-z][a-z0-9_]*_[a-z][a-z0-9_]*$/);
      for (const check of table.checks ?? []) {
        if (!/^chk_[a-z0-9]+(?:_[a-z0-9]+)*$/.test(check.name)) {
          report({
            rule: naming.name,
            identifier: `${identifier(table)}.${check.name}`,
            message: 'check must be named chk_<description>',
          });
        }
      }
      const references = new Map();
      for (const column of table.columns) {
        for (const reference of column.references ?? []) {
          const refs = references.get(reference.name) ?? [];
          refs.push({ ordinal: column.ordinalPosition, reference });
          references.set(reference.name, refs);
        }
      }
      for (const [name, refs] of references) {
        // extract-pg-schema omits conkey ordinal positions; native SQL checks
        // composite foreign-key order without guessing from local columns.
        if (refs.length > 1) continue;
        const expected = `fk_${refs[0].reference.tableName}_${refs[0].reference.columnName}`;
        if (name !== expected) {
          report({
            rule: naming.name,
            identifier: `${identifier(table)}.${name}`,
            message: `foreign key must be named ${expected}`,
          });
        }
      }
      for (const trigger of table.triggers ?? []) {
        const expectedPrefix = `trg_${table.name}_`;
        if (!new RegExp(`^${expectedPrefix}[a-z0-9_]+$`).test(trigger.name)) {
          report({
            rule: naming.name,
            identifier: `${identifier(table)}.${trigger.name}`,
            message: 'trigger must be named trg_<table>_<description>',
          });
        }
      }
    }
    for (const view of [...schemaObject.views, ...(schemaObject.materializedViews ?? [])]) {
      reportName(report, view, naming.name, /^[a-z][a-z0-9_]*_vw_[a-z][a-z0-9_]*$/);
    }
    for (const enumType of schemaObject.enums) {
      reportName(report, enumType, naming.name, /^[a-z][a-z0-9_]*_[a-z][a-z0-9_]*$/);
    }
    for (const fn of schemaObject.functions) {
      reportName(report, fn, naming.name, /^[a-z][a-z0-9_]*_fn_[a-z][a-z0-9_]*$/);
    }
    for (const procedure of schemaObject.procedures) {
      reportName(report, procedure, naming.name, /^[a-z][a-z0-9_]*_sp_[a-z][a-z0-9_]*$/);
    }
  },
};

module.exports = naming;
