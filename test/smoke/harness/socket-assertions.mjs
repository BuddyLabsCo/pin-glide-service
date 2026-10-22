import { WebSocketClosedError } from './websocket.mjs';

export async function expectNoMessage(socket, description, timeoutMs = 300) {
  try {
    const message = await socket.next(timeoutMs);
    const type = message.json?.type;
    const frame = typeof type === 'string' && /^[a-z_]{1,32}$/.test(type) ? ` ${type}` : '';
    throw new Error(`${description}: received an unexpected WebSocket frame${frame}`);
  } catch (error) {
    if (error instanceof WebSocketClosedError) throw error;
    if (!String(error.message).includes('timed out waiting')) throw error;
  }
  return true;
}

export async function expectSocketClose(socket, code) {
  try {
    await socket.next(5_000);
  } catch (error) {
    if (error instanceof WebSocketClosedError && error.code === code) return error;
    throw error;
  }
  throw new Error(`WebSocket stayed open instead of closing with ${code}`);
}
