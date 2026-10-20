import { randomUUID } from 'node:crypto';

import { serverRelativePath } from './path.mjs';
import { assertHttpOrigin } from './target.mjs';

export class SmokeProblem extends Error {
  constructor(status, body, requestID) {
    super(`smoke request failed with HTTP ${status}${body?.code ? ` ${body.code}` : ''}`);
    this.name = 'SmokeProblem';
    this.status = status;
    this.code = body?.code || '';
    this.requestId = requestID || body?.requestId || '';
    this.detail = body?.detail || body?.title || '';
  }
}

export class SmokeHttp {
  constructor({ origin, fetchImpl = fetch, timeoutMs = 15_000 } = {}) {
    this.origin = assertHttpOrigin(origin);
    this.fetchImpl = fetchImpl;
    this.timeoutMs = timeoutMs;
  }

  call(options) {
    return this.dispatch(options, false);
  }

  callDropped(options) {
    return this.dispatch(options, true);
  }

  async dispatch({ method = 'GET', path, token, body, headers = {} } = {}, discardResponse) {
    const route = serverRelativePath(path, 'HTTP');
    const url = new URL(route, this.origin);
    if (url.origin !== this.origin) {
      throw new Error('Smoke HTTP path must remain on the adopted origin');
    }
    const requestHeaders = {
      accept: 'application/json, application/problem+json',
      'x-request-id': randomUUID(),
      ...headers,
    };
    if (token) requestHeaders.authorization = `Bearer ${token}`;
    let requestBody;
    if (body !== undefined) {
      requestHeaders['content-type'] = 'application/json';
      requestBody = JSON.stringify(body);
    }

    let response;
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.timeoutMs);
    timer.unref();
    try {
      try {
        response = await this.fetchImpl(url, {
          method,
          headers: requestHeaders,
          body: requestBody,
          signal: controller.signal,
        });
      } catch (error) {
        throw new Error(`smoke HTTP ${method} ${route} failed: ${error.message}`);
      }

      const headerRequestID = response.headers.get('x-request-id');
      const requestID = headerRequestID || requestHeaders['x-request-id'];
      if (discardResponse && response.ok) {
        controller.abort();
        return { status: response.status, requestId: requestID, cacheControl: response.headers.get('cache-control') };
      }
      const text = await response.text();
      let payload;
      if (text) {
        try {
          payload = JSON.parse(text);
        } catch {
          payload = undefined;
        }
      }
      const resolvedRequestID = headerRequestID || payload?.requestId || requestHeaders['x-request-id'];
      if (!response.ok) throw new SmokeProblem(response.status, payload, resolvedRequestID);
      return {
        status: response.status,
        requestId: resolvedRequestID,
        cacheControl: response.headers.get('cache-control'),
        body: payload,
      };
    } finally {
      clearTimeout(timer);
    }
  }
}
