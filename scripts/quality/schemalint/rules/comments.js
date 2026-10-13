const objectIdentifier = (object) => `${object.schemaName}.${object.name}`;
const hasText = (value) => typeof value === 'string' && value.trim().length > 0;

const comments = {
  name: 'pin-glide-comments',
  docs: { description: 'Require non-empty object, column, and parameter comments.' },
  process({ schemaObject, report }) {
    const checkComment = (object) => {
      if (!hasText(object.comment)) {
        report({
          rule: comments.name,
          identifier: objectIdentifier(object),
          message: 'object must have a non-empty COMMENT ON',
        });
      }
    };
    for (const enumType of schemaObject.enums) checkComment(enumType);
    for (const view of [...schemaObject.views, ...(schemaObject.materializedViews ?? [])]) checkComment(view);
    for (const table of schemaObject.tables) {
      checkComment(table);
      for (const column of table.columns) {
        if (!hasText(column.comment)) {
          report({
            rule: comments.name,
            identifier: `${objectIdentifier(table)}.${column.name}`,
            message: 'column must have a non-empty COMMENT ON',
          });
        }
      }
    }
    for (const routine of [...schemaObject.functions, ...schemaObject.procedures]) {
      checkComment(routine);
      const parameters = routine.parameters ?? [];
      if (parameters.length === 0) continue;
      const lines = String(routine.comment ?? '').split(/\r?\n/);
      const hasParametersSection = lines.some((line) => line.trim().toLowerCase() === 'parameters:');
      if (!hasParametersSection) {
        report({
          rule: comments.name,
          identifier: objectIdentifier(routine),
          message: 'routine comments with parameters must include a parameters: line',
        });
        continue;
      }
      for (const parameter of parameters) {
        const name = parameter.name;
        const pattern = new RegExp(`^\\s*${name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}:\\s*\\S`);
        if (!lines.some((line) => pattern.test(line))) {
          report({
            rule: comments.name,
            identifier: objectIdentifier(routine),
            message: `parameter comments must include ${name}:`,
          });
        }
      }
    }
  },
};

module.exports = comments;
