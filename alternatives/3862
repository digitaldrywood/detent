const crypto = require("node:crypto");
const http = require("node:http");

const clientId = "detent-playwright";
const clientSecret = "detent-playwright-secret";
const accessToken = "fake-provider-access-token";

async function startFakeOIDC() {
  const { privateKey, publicKey } = crypto.generateKeyPairSync("rsa", {
    modulusLength: 2048,
  });
  const transactions = new Map();
  const codes = new Map();
  let issuerURL = "";
  let lastCode = "";

  const server = http.createServer(async (request, response) => {
    const requestURL = new URL(request.url, issuerURL);
    if (requestURL.pathname === "/.well-known/openid-configuration") {
      writeJSON(response, 200, {
        issuer: issuerURL,
        authorization_endpoint: `${issuerURL}/authorize`,
        token_endpoint: `${issuerURL}/token`,
        jwks_uri: `${issuerURL}/jwks`,
        id_token_signing_alg_values_supported: ["RS256"],
      });
      return;
    }
    if (requestURL.pathname === "/jwks") {
      const jwk = publicKey.export({ format: "jwk" });
      writeJSON(response, 200, {
        keys: [{ ...jwk, kid: "playwright", use: "sig", alg: "RS256" }],
      });
      return;
    }
    if (requestURL.pathname === "/authorize") {
      const transaction = authorizationTransaction(requestURL);
      if (!transaction) {
        writeJSON(response, 400, { error: "invalid_request" });
        return;
      }
      const id = crypto.randomUUID();
      transactions.set(id, transaction);
      response.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
      response.end(`<!doctype html>
<html lang="en">
  <head><meta charset="utf-8"><title>Fake identity provider</title></head>
  <body>
    <main>
      <h1>Test identity provider</h1>
      <p>Choose an account for this isolated OIDC test.</p>
      <a href="/approve?id=${encodeURIComponent(id)}&identity=allowed">Sign in as operator</a>
      <a href="/approve?id=${encodeURIComponent(id)}&identity=denied">Sign in as outsider</a>
    </main>
  </body>
</html>`);
      return;
    }
    if (requestURL.pathname === "/approve") {
      const id = requestURL.searchParams.get("id");
      const transaction = transactions.get(id);
      if (!transaction) {
        writeJSON(response, 400, { error: "invalid_request" });
        return;
      }
      transactions.delete(id);
      const code = crypto.randomBytes(24).toString("base64url");
      lastCode = code;
      codes.set(code, {
        ...transaction,
        email:
          requestURL.searchParams.get("identity") === "allowed"
            ? "operator@example.com"
            : "outsider@example.net",
      });
      const callback = new URL(transaction.redirectURI);
      callback.searchParams.set("code", code);
      callback.searchParams.set("state", transaction.state);
      response.writeHead(303, { Location: callback.toString() });
      response.end();
      return;
    }
    if (requestURL.pathname === "/token" && request.method === "POST") {
      const body = await requestBody(request);
      const form = new URLSearchParams(body);
      const credentials = clientCredentials(request, form);
      const code = form.get("code");
      const transaction = codes.get(code);
      if (
        credentials.id !== clientId ||
        credentials.secret !== clientSecret ||
        !transaction ||
        form.get("grant_type") !== "authorization_code" ||
        form.get("redirect_uri") !== transaction.redirectURI ||
        pkceChallenge(form.get("code_verifier")) !== transaction.codeChallenge
      ) {
        writeJSON(response, 400, { error: "invalid_grant" });
        return;
      }
      codes.delete(code);
      const now = Math.floor(Date.now() / 1000);
      writeJSON(response, 200, {
        access_token: accessToken,
        token_type: "Bearer",
        expires_in: 3600,
        id_token: signIDToken(privateKey, {
          iss: issuerURL,
          sub: `subject:${transaction.email}`,
          aud: clientId,
          iat: now,
          exp: now + 3600,
          nonce: transaction.nonce,
          email: transaction.email,
          email_verified: true,
        }),
      });
      return;
    }
    response.writeHead(404);
    response.end();
  });

  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();
  issuerURL = `http://127.0.0.1:${address.port}`;

  return {
    url: issuerURL,
    clientId,
    clientSecret,
    accessToken,
    lastCode() {
      return lastCode;
    },
    async stop() {
      await new Promise((resolve) => server.close(resolve));
    },
  };
}

function authorizationTransaction(requestURL) {
  const scopes = new Set((requestURL.searchParams.get("scope") || "").split(" "));
  if (
    requestURL.searchParams.get("client_id") !== clientId ||
    requestURL.searchParams.get("response_type") !== "code" ||
    requestURL.searchParams.get("code_challenge_method") !== "S256" ||
    !requestURL.searchParams.get("code_challenge") ||
    !requestURL.searchParams.get("state") ||
    !requestURL.searchParams.get("nonce") ||
    !scopes.has("openid") ||
    !scopes.has("email")
  ) {
    return null;
  }
  return {
    redirectURI: requestURL.searchParams.get("redirect_uri"),
    codeChallenge: requestURL.searchParams.get("code_challenge"),
    state: requestURL.searchParams.get("state"),
    nonce: requestURL.searchParams.get("nonce"),
  };
}

function clientCredentials(request, form) {
  const authorization = request.headers.authorization || "";
  if (authorization.startsWith("Basic ")) {
    const decoded = Buffer.from(authorization.slice(6), "base64").toString("utf8");
    const separator = decoded.indexOf(":");
    return {
      id: decodeURIComponent(decoded.slice(0, separator)),
      secret: decodeURIComponent(decoded.slice(separator + 1)),
    };
  }
  return { id: form.get("client_id"), secret: form.get("client_secret") };
}

function pkceChallenge(verifier) {
  if (!verifier) {
    return "";
  }
  return crypto.createHash("sha256").update(verifier).digest("base64url");
}

function signIDToken(privateKey, claims) {
  const header = Buffer.from(
    JSON.stringify({ alg: "RS256", kid: "playwright", typ: "JWT" }),
  ).toString("base64url");
  const payload = Buffer.from(JSON.stringify(claims)).toString("base64url");
  const unsigned = `${header}.${payload}`;
  const signature = crypto.sign("RSA-SHA256", Buffer.from(unsigned), privateKey);
  return `${unsigned}.${signature.toString("base64url")}`;
}

function requestBody(request) {
  return new Promise((resolve, reject) => {
    let body = "";
    request.setEncoding("utf8");
    request.on("data", (chunk) => {
      body += chunk;
    });
    request.on("end", () => resolve(body));
    request.on("error", reject);
  });
}

function writeJSON(response, status, value) {
  response.writeHead(status, { "Content-Type": "application/json" });
  response.end(JSON.stringify(value));
}

module.exports = { startFakeOIDC };
