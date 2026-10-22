import { randomUUID } from 'node:crypto';

import { serverRelativePath } from './path.mjs';
import { assertHttpOrigin } from './target.mjs';

export class SmokeProblem extends Error {
  constructor(status, body, requestID) {
    super(`smoke request failed with HTTP ${status}${body?.code ? ` ${body.code}` : ''}`);
    this.name = 'SmokeProblem';
    this.status = status;
    this.code = body?.code || '';
    this.type = body?.type || '';
    this.title = body?.title || '';
    this.requestId = requestID || body?.requestId || '';
    this.bodyRequestId = body?.requestId || '';
    this.bodyStatus = body?.status;
    this.detail = body?.detail || '';
    this.body = body;
  }
}

export class SmokeBlocked extends Error {
  constructor(message) {
    super(message);
    this.name = 'SmokeBlocked';
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

  // Start a request whose caller can abandon it before the service answers.
  // The server observes that transport cancellation; the delivery work it
  // already admitted is expected to continue.
  callAbandoned(options) {
    const controller = new AbortController();
    controller.abandoned = false;
    return {
      request: this.dispatch(options, false, controller),
      abort: () => {
        controller.abandoned = true;
        controller.abort();
      },
      aborted: () => controller.abandoned,
    };
  }

  async dispatch(
    { method = 'GET', path, token, body, rawBody, headers = {}, timeoutMs = this.timeoutMs } = {},
    discardResponse,
    controller = new AbortController(),
  ) {
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
    if (rawBody !== undefined && body !== undefined) {
      throw new Error('Smoke HTTP requests cannot carry both body and rawBody');
    }
    if (rawBody !== undefined) {
      requestHeaders['content-type'] = 'application/json';
      requestBody = rawBody;
    } else if (body !== undefined) {
      requestHeaders['content-type'] = 'application/json';
      requestBody = JSON.stringify(body);
    }

    let response;
    const timer = setTimeout(() => controller.abort(), timeoutMs);
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
        const failure = new Error(`smoke HTTP ${method} ${route} failed: ${error.message}`);
        failure.aborted = controller.abandoned === true;
        throw failure;
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
