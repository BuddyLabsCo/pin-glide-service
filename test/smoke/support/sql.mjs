export function escapeSQLString(value) {
  return `'${String(value).replaceAll("'", "''")}'`;
}

export function sqlTextArray(values) {
  if (values.length === 0) return 'ARRAY[]::text[]';
  return `ARRAY[${values.map(escapeSQLString).join(', ')}]::text[]`;
}
