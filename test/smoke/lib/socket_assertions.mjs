import { WebSocketClosedError } from './websocket.mjs';

export async function expectNoMessage(socket, description, timeoutMs = 300) {
  try {
    const message = await socket.next(timeoutMs);
    throw new Error(`${description}: ${message.text}`);
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
