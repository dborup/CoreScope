'use strict';
/* Helpers for E2E suites that run the real ingestor and server binaries
 * against a temp copy of the fixture DB, behind a minimal in-process MQTT
 * 3.1.1 broker (CONNECT, SUBSCRIBE, PINGREQ, DISCONNECT, and QoS 0 PUBLISH
 * towards subscribers). Every spawned process gets its own log file so a
 * wait can never match a line written by an earlier process.
 */

const { spawn } = require('child_process');
const fs = require('fs');
const net = require('net');
const path = require('path');

function encodeLength(n) {
  const out = [];
  do {
    let byte = n % 128;
    n = Math.floor(n / 128);
    if (n > 0) byte |= 0x80;
    out.push(byte);
  } while (n > 0);
  return Buffer.from(out);
}

function startFakeBroker() {
  return new Promise((resolve) => {
    const sockets = new Set();
    const subscribers = new Set();
    const server = net.createServer((sock) => {
      sockets.add(sock);
      sock.on('close', () => { sockets.delete(sock); subscribers.delete(sock); });
      sock.on('error', () => {});
      let buf = Buffer.alloc(0);
      sock.on('data', (d) => {
        buf = Buffer.concat([buf, d]);
        for (;;) {
          if (buf.length < 2) return;
          let len = 0;
          let mult = 1;
          let i = 1;
          let complete = false;
          for (; i < buf.length && i <= 4; i++) {
            len += (buf[i] & 0x7f) * mult;
            mult *= 128;
            if ((buf[i] & 0x80) === 0) { complete = true; i++; break; }
          }
          if (!complete || buf.length < i + len) return;
          const type = buf[0] >> 4;
          const body = buf.slice(i, i + len);
          buf = buf.slice(i + len);
          if (type === 1) sock.write(Buffer.from([0x20, 0x02, 0x00, 0x00])); // CONNACK accepted
          else if (type === 8) { // SUBSCRIBE → SUBACK granting QoS 0 per topic
            let n = 0;
            for (let p = 2; p + 2 <= body.length;) {
              p += 2 + body.readUInt16BE(p) + 1;
              n++;
            }
            sock.write(Buffer.from([0x90, 2 + n, body[0], body[1]].concat(new Array(n).fill(0))));
            subscribers.add(sock);
          } else if (type === 12) sock.write(Buffer.from([0xd0, 0x00])); // PINGRESP
          else if (type === 14) sock.end();
        }
      });
    });
    server.listen(0, '127.0.0.1', () => resolve({
      port: server.address().port,
      // QoS 0 PUBLISH to every subscribed client; topic filters are not
      // evaluated, the suites subscribe to meshcore/#.
      publish: (topic, payload) => {
        const t = Buffer.from(topic);
        const p = Buffer.from(payload);
        const varHeader = Buffer.concat([Buffer.from([t.length >> 8, t.length & 0xff]), t]);
        const packet = Buffer.concat([Buffer.from([0x30]), encodeLength(varHeader.length + p.length), varHeader, p]);
        subscribers.forEach((s) => s.write(packet));
        return subscribers.size;
      },
      close: () => { sockets.forEach((s) => s.destroy()); server.close(); },
    }));
  });
}

function freePort() {
  return new Promise((resolve, reject) => {
    const s = net.createServer();
    s.listen(0, '127.0.0.1', () => { const p = s.address().port; s.close(() => resolve(p)); });
    s.on('error', reject);
  });
}

function createProcessGroup(dir) {
  const children = new Set();
  let startCount = 0;
  return {
    children,
    start(label, bin, args) {
      const logFile = path.join(dir, label + '-' + (++startCount) + '.log');
      const out = fs.openSync(logFile, 'w');
      const child = spawn(bin, args, { stdio: ['ignore', out, out] });
      fs.closeSync(out);
      child.label = label;
      child.logFile = logFile;
      children.add(child);
      child.once('exit', () => children.delete(child));
      return child;
    },
    async stopAll() {
      for (const child of Array.from(children)) await stopProcess(child);
    },
  };
}

async function stopProcess(child) {
  if (!child || child.exitCode !== null) return;
  const exited = new Promise((r) => child.once('exit', r));
  child.kill('SIGTERM');
  const t = setTimeout(() => child.kill('SIGKILL'), 5000);
  await exited;
  clearTimeout(t);
}

async function waitFor(what, fn, timeoutMs) {
  const deadline = Date.now() + (timeoutMs || 20000);
  let last;
  while (Date.now() < deadline) {
    try {
      const v = await fn();
      if (v) return v;
    } catch (e) { last = e; }
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error('timed out waiting for ' + what + (last ? ': ' + last.message : ''));
}

function logText(child) {
  try { return fs.readFileSync(child.logFile, 'utf8'); } catch (e) { return ''; }
}

module.exports = { startFakeBroker, freePort, createProcessGroup, stopProcess, waitFor, logText };
