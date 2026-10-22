function required(value, message) {
  if (value === undefined || value === null || value === '') throw new Error(message);
  return value;
}

function database(url) {
  let parsed;
  try {
    parsed = new URL(required(url, 'Production database URL is missing'));
  } catch {
    throw new Error('Production database URL is invalid');
  }
  if (!['postgres:', 'postgresql:'].includes(parsed.protocol)) {
    throw new Error('Production database URL must be PostgreSQL');
  }
  const name = parsed.pathname.replace(/^\//, '');
  if (name !== 'pin_glide') throw new Error(`Compose database must be pin_glide, not ${name || 'empty'}`);
  if (parsed.hostname !== 'postgres') throw new Error('Compose database must use the postgres service');
  if (parsed.username !== 'pin_glide') throw new Error('Compose database user must be pin_glide');
  return name;
}

function onePort(service, target) {
  const ports = service?.ports;
  if (!Array.isArray(ports) || ports.length !== 1) {
    throw new Error('API must publish exactly one host port');
  }
  const port = ports[0];
  if (Number(port.target) !== target.containerPort) {
    throw new Error(`API container port must remain ${target.containerPort}`);
  }
  if (String(port.published) !== String(target.hostPort) || (port.protocol && port.protocol !== 'tcp')) {
    throw new Error(`API host port must be ${target.hostPort}`);
  }
  if (port.host_ip !== target.hostIP) {
    throw new Error(`API host port must bind ${target.hostIP}, not ${port.host_ip || 'all interfaces'}`);
  }
  return String(port.published);
}

export function summarizeCompose(config, target, release) {
  if (config?.name !== target.project) {
    throw new Error(`Compose project must be ${target.project}`);
  }
  const services = config.services || {};
  const unexpected = Object.keys(services).filter(name => !['api', 'db-tools', 'postgres'].includes(name));
  if (unexpected.length > 0) {
    throw new Error(`Compose project contains unrelated service ${unexpected.join(', ')}`);
  }
  const api = required(services.api, 'Compose API service is missing');
  const dbTools = required(services['db-tools'], 'Compose database-tools service is missing');
  const postgres = required(services.postgres, 'Compose PostgreSQL service is missing');
  const apiImage = `ghcr.io/buddylabsco/pin-glide-api:${release}`;
  const dbToolsImage = `ghcr.io/buddylabsco/pin-glide-db-tools:${release}`;
  const postgresImage = 'ghcr.io/buddylabsco/pin-glide-postgres:18-1.6.8';
  if (api.image !== apiImage) throw new Error('API must use the selected immutable release image');
  if (dbTools.image !== dbToolsImage) throw new Error('Database tools must use the selected immutable release image');
  if (postgres.image !== postgresImage) throw new Error('PostgreSQL must use the pinned pg_cron image');
  if (postgres.environment?.POSTGRES_DB !== target.database) {
    throw new Error(`PostgreSQL must own the ${target.database} database`);
  }
  if (postgres.environment?.POSTGRES_USER !== 'pin_glide') {
    throw new Error('PostgreSQL must use the pin_glide owner');
  }
  required(postgres.environment?.POSTGRES_PASSWORD, 'PostgreSQL password is not configured');
  if (Array.isArray(postgres.ports) && postgres.ports.length > 0) {
    throw new Error('PostgreSQL must not publish a host port');
  }
  if (api.environment?.PIN_GLIDE_ENV !== 'production' || dbTools.environment?.PIN_GLIDE_ENV !== 'production') {
    throw new Error('Packaged API and database tools must use the production startup contract');
  }
  if (api.environment?.PIN_GLIDE_API_ADDRESS !== ':8080') {
    throw new Error('API container listener must remain :8080');
  }
  const apiDatabase = database(api.environment?.PIN_GLIDE_DATABASE_URL);
  const toolsDatabase = database(dbTools.environment?.PIN_GLIDE_DATABASE_URL);
  if (apiDatabase !== target.database || toolsDatabase !== target.database) {
    throw new Error(`Compose database must be ${target.database}`);
  }
  required(api.environment?.PIN_GLIDE_BAILIAN_API_KEY, 'Bailian parent key is not configured');
  const publishedPort = onePort(api, target);
  return {
    project: config.name,
    apiAddress: api.environment.PIN_GLIDE_API_ADDRESS,
    publishedPort,
    database: apiDatabase,
    images: {
      api: apiImage,
      dbTools: dbToolsImage,
      postgres: postgresImage,
    },
    providers: {
      wechat: Boolean(api.environment.PIN_GLIDE_WECHAT_APP_ID && api.environment.PIN_GLIDE_WECHAT_APP_SECRET),
      qq: Boolean(api.environment.PIN_GLIDE_QQ_APP_ID && api.environment.PIN_GLIDE_QQ_APP_KEY),
    },
  };
}
