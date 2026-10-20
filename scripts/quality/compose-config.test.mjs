import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import test from 'node:test';

const compose = fileURLToPath(new URL('../../deploy/compose/docker-compose.yml', import.meta.url));
const providerVariables = [
  'WECHAT_APP_ID',
  'WECHAT_APP_SECRET',
  'QQ_APP_ID',
  'QQ_APP_KEY',
];

function configured(provider) {
  const env = {
    ...process.env,
    COMPOSE_PROJECT_NAME: 'pin-glide-config-test',
    RELEASE: '0.1.0',
    POSTGRES_USER: 'pin_glide',
    POSTGRES_PASSWORD: 'test-password',
    BAILIAN_API_KEY: 'test-parent-key',
  };
  for (const name of providerVariables) delete env[name];
  Object.assign(env, provider);

  const result = spawnSync('docker', [
    'compose', '--env-file', '/dev/null', '-f', compose, 'config', '--format', 'json',
  ], { env, encoding: 'utf8' });
  assert.equal(result.status, 0, result.stderr);
  return JSON.parse(result.stdout).services.api;
}

test('Compose interpolates every supported identity-provider startup matrix', () => {
  const cases = [
    {
      name: 'neither provider configured',
      env: {},
      want: {},
    },
    {
      name: 'only WeChat configured',
      env: {
        WECHAT_APP_ID: 'wx-app',
        WECHAT_APP_SECRET: 'wx-secret',
      },
      want: {
        PIN_GLIDE_WECHAT_APP_ID: 'wx-app',
        PIN_GLIDE_WECHAT_APP_SECRET: 'wx-secret',
      },
    },
    {
      name: 'only QQ configured',
      env: {
        QQ_APP_ID: 'qq-app',
        QQ_APP_KEY: 'qq-app-key',
      },
      want: {
        PIN_GLIDE_QQ_APP_ID: 'qq-app',
        PIN_GLIDE_QQ_APP_KEY: 'qq-app-key',
      },
    },
    {
      name: 'both providers configured',
      env: {
        WECHAT_APP_ID: 'wx-app',
        WECHAT_APP_SECRET: 'wx-secret',
        QQ_APP_ID: 'qq-app',
        QQ_APP_KEY: 'qq-app-key',
      },
      want: {
        PIN_GLIDE_WECHAT_APP_ID: 'wx-app',
        PIN_GLIDE_WECHAT_APP_SECRET: 'wx-secret',
        PIN_GLIDE_QQ_APP_ID: 'qq-app',
        PIN_GLIDE_QQ_APP_KEY: 'qq-app-key',
      },
    },
    {
      name: 'WeChat partially configured',
      env: {
        WECHAT_APP_ID: 'wx-app',
      },
      want: {
        PIN_GLIDE_WECHAT_APP_ID: 'wx-app',
      },
    },
    {
      name: 'QQ partially configured',
      env: {
        QQ_APP_KEY: 'qq-app-key',
      },
      want: {
        PIN_GLIDE_QQ_APP_KEY: 'qq-app-key',
      },
    },
  ];

  const variables = [
    'PIN_GLIDE_WECHAT_APP_ID',
    'PIN_GLIDE_WECHAT_APP_SECRET',
    'PIN_GLIDE_QQ_APP_ID',
    'PIN_GLIDE_QQ_APP_KEY',
  ];
  for (const item of cases) {
    const api = configured(item.env);
    for (const name of variables) {
      assert.equal(api.environment[name] ?? '', item.want[name] ?? '', `${item.name}: ${name}`);
    }
  }
});

test('Compose publishes the adopted LAN API port while keeping the container listener unchanged', () => {
  const api = configured({});
  assert.equal(api.environment.PIN_GLIDE_API_ADDRESS, ':8080');
  assert.deepEqual(api.ports, [{
    mode: 'ingress',
    target: 8080,
    published: '33489',
    protocol: 'tcp',
  }]);
});
