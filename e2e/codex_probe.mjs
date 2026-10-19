#!/usr/bin/env node
// The stand-in client of the Codex rehearsal's cross-provider arms, run in the client
// container (node, no curl or python). Started by redcoast-client in place of claude or
// codex, with PROBE_HOLD set to claude or to the name of a Codex process, it writes the
// session credential it was given to /client/out/<PROBE_HOLD>.token and holds the session
// until the container is removed. As "codex_probe.mjs probe <base URL> <path> <proxy
// host:port>" it reads a credential on stdin, so that it is in no command line, sends one
// inference to the reverse proxy and one CONNECT to the forward proxy with it, and prints
// both statuses.
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import http from "node:http";
import net from "node:net";


function post(base, path, token) {
  const body = path.endsWith("/responses")
    ? { model: "gpt-5.5", instructions: "", stream: true, input: [{ type: "message", role: "user", content: [{ type: "input_text", text: "Hi" }] }] }
    : { model: "claude-sonnet-4-5", max_tokens: 8, messages: [{ role: "user", content: "Hi" }] };
  const data = JSON.stringify(body);
  return new Promise((resolve) => {
    const request = http.request(new URL(path, base), {
      method: "POST",
      headers: { authorization: `Bearer ${token}`, "content-type": "application/json", "content-length": Buffer.byteLength(data) },
    }, (response) => {
      response.resume();
      response.on("end", () => resolve(String(response.statusCode)));
      response.on("error", () => resolve(String(response.statusCode)));
    });
    request.on("error", () => resolve("none"));
    request.setTimeout(10000, () => request.destroy());
    request.end(data);
  });
}

function connect(hostport, token) {
  const [host, port] = [hostport.slice(0, hostport.lastIndexOf(":")), Number(hostport.slice(hostport.lastIndexOf(":") + 1))];
  const authorization = Buffer.from(`session:${token}`).toString("base64");
  return new Promise((resolve) => {
    const socket = net.connect(port, host, () => {
      socket.write(`CONNECT example.test:443 HTTP/1.1\r\nHost: example.test:443\r\nProxy-Authorization: Basic ${authorization}\r\n\r\n`);
    });
    let head = "";
    socket.on("data", (chunk) => {
      head += chunk.toString("latin1");
      if (head.includes("\r\n")) {
        socket.destroy();
        resolve(head.split(" ")[1] ?? "none");
      }
    });
    socket.on("error", () => resolve("none"));
    socket.setTimeout(10000, () => socket.destroy());
    socket.on("close", () => resolve("none"));
  });
}

if (process.argv[2] === "probe") {
  const [base, path, proxy] = process.argv.slice(3);
  const token = readFileSync(0, "utf8").trim();
  const status = await post(base, path, token);
  console.log(`post=${status} connect=${await connect(proxy, token)}`);
} else {
  const name = process.env.PROBE_HOLD;
  mkdirSync("/client/out", { recursive: true });
  writeFileSync(`/client/out/${name}.token`, process.env[name === "claude" ? "CLAUDE_CODE_OAUTH_TOKEN" : "CODEX_GATEWAY_TOKEN"] ?? "", { mode: 0o600 });
  setInterval(() => {}, 60000);
}
