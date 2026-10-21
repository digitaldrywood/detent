const net = require("node:net");

async function startFakeSMTP() {
  const messages = [];
  const waiters = [];
  const sockets = new Set();
  const server = net.createServer((socket) => {
    sockets.add(socket);
    socket.setEncoding("utf8");
    socket.write("220 detent.test ESMTP\r\n");
    let buffer = "";
    let data = null;

    socket.on("data", (chunk) => {
      buffer += chunk;
      for (;;) {
        const newline = buffer.indexOf("\r\n");
        if (newline < 0) {
          break;
        }
        let line = buffer.slice(0, newline);
        buffer = buffer.slice(newline + 2);
        if (data !== null) {
          if (line === ".") {
            const message = data.join("\r\n");
            data = null;
            const waiter = waiters.shift();
            if (waiter) {
              waiter.resolve(message);
            } else {
              messages.push(message);
            }
            socket.write("250 2.0.0 queued\r\n");
            continue;
          }
          if (line.startsWith("..")) {
            line = line.slice(1);
          }
          data.push(line);
          continue;
        }

        const command = line.toUpperCase();
        if (command.startsWith("EHLO")) {
          socket.write("250-detent.test\r\n250 8BITMIME\r\n");
        } else if (command.startsWith("HELO") || command.startsWith("MAIL FROM:") || command.startsWith("RCPT TO:")) {
          socket.write("250 2.0.0 ok\r\n");
        } else if (command === "DATA") {
          data = [];
          socket.write("354 End data with <CR><LF>.<CR><LF>\r\n");
        } else if (command === "QUIT") {
          socket.end("221 2.0.0 bye\r\n");
        } else {
          socket.write("250 2.0.0 ok\r\n");
        }
      }
    });
    socket.on("close", () => sockets.delete(socket));
  });

  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();

  return {
    address: `${address.address}:${address.port}`,
    messageCount() {
      return messages.length;
    },
    waitForMessage(timeout = 10_000) {
      if (messages.length > 0) {
        return Promise.resolve(messages.shift());
      }
      return new Promise((resolve, reject) => {
        const waiter = {
          resolve(message) {
            clearTimeout(timer);
            resolve(message);
          },
        };
        const timer = setTimeout(() => {
          const index = waiters.indexOf(waiter);
          if (index >= 0) {
            waiters.splice(index, 1);
          }
          reject(new Error("Timed out waiting for SMTP message"));
        }, timeout);
        waiters.push(waiter);
      });
    },
    async stop() {
      for (const socket of sockets) {
        socket.destroy();
      }
      await new Promise((resolve, reject) => {
        server.close((error) => {
          if (error) {
            reject(error);
          } else {
            resolve();
          }
        });
      });
    },
  };
}

module.exports = { startFakeSMTP };
