export function serverRelativePath(value, surface) {
  if (
    typeof value !== 'string'
    || !value.startsWith('/')
    || value.startsWith('//')
    || value.includes('\\')
    || /[\t\r\n]/.test(value)
  ) {
    throw new Error(`Smoke ${surface} paths must be server-relative`);
  }
  if (value.includes('?') || value.includes('#')) {
    throw new Error(`Smoke ${surface} paths must not carry credentials or fragments in the URL`);
  }
  return value;
}
