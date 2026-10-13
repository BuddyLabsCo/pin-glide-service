/** @type {import('schemalint').Config} */
module.exports = {
  connection: {
    host: process.env.PGHOST || '127.0.0.1',
    port: Number(process.env.PGPORT || 5432),
    user: process.env.PGUSER || 'pin_glide',
    password: process.env.PGPASSWORD || 'pin_glide',
    database: process.env.PGDATABASE || 'pin_glide',
  },
  schemas: [{ name: 'release' }, { name: 'pin_glide' }],
  plugins: ['./scripts/quality/schemalint/rules'],
  rules: {
    'pin-glide-object-naming': ['error'],
    'pin-glide-comments': ['error'],
  },
};
