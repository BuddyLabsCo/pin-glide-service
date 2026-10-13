/** @type {import('schemalint').Config} */

// scripts/db/psqldef.sh exports these for the disposable database. Refusing to
// guess keeps a schemalint run outside `make lint-sql` from silently linting a
// developer's own database.
const disposable = (name) => {
  const value = process.env[name];
  if (!value) {
    throw new Error(`${name} must point at the disposable database; run make lint-sql`);
  }
  return value;
};

module.exports = {
  connection: {
    host: disposable('PGHOST'),
    port: Number(disposable('PGPORT')),
    user: disposable('PGUSER'),
    password: disposable('PGPASSWORD'),
    database: disposable('PGDATABASE'),
  },
  schemas: [{ name: 'release' }, { name: 'pin_glide' }],
  plugins: ['./scripts/quality/schemalint/rules'],
  rules: {
    'pin-glide-object-naming': ['error'],
    'pin-glide-comments': ['error'],
  },
};
