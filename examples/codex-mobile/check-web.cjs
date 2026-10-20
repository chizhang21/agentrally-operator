// Check rejected passwords, session cookies and the authenticated web UI.
const http = require("node:http");
const request = (method, path, payload, cookie) => new Promise((resolve, reject) => {
  const body = payload ? JSON.stringify(payload) : "";
  const headers = {Host: "agent.example.com", "Content-Type": "application/json", "Content-Length": Buffer.byteLength(body)};
  if (cookie) headers.Cookie = cookie;
  const req = http.request({host: "127.0.0.1", port: 5900, method, path, headers}, res => {
    let text = "";
    res.setEncoding("utf8");
    res.on("data", part => { text += part; });
    res.on("end", () => resolve({status: res.statusCode, headers: res.headers, body: text}));
  });
  req.setTimeout(5000, () => req.destroy(new Error("Request timed out")));
  req.on("error", reject);
  req.end(body);
});
(async () => {
  const bad = await request("POST", "/auth/login", {password: "wrong"});
  if (bad.status !== 401) throw new Error("Wrong password was not rejected");
  const good = await request("POST", "/auth/login", {password: "smoke-test-only"});
  const cookie = good.headers["set-cookie"]?.[0];
  if (good.status !== 200 || !cookie) throw new Error("Login failed");
  const page = await request("GET", "/", null, cookie.split(";")[0]);
  if (page.status !== 200 || !page.body.includes("id=\"app\"")) throw new Error("Web UI unavailable");
})().catch(error => { console.error(error.message); process.exit(1); });
