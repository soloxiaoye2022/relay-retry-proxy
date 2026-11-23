import json, sys, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MODE = {"n": 0}
LOCK = threading.Lock()

class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_a):
        pass

    def do_GET(self):
        self._handle()

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        if length:
            self.rfile.read(length)
        self._handle()

    def _handle(self):
        with LOCK:
            MODE["n"] += 1
            n = MODE["n"]
        print(f"[stub] request #{n} mode=retry-stream", flush=True)
        if n == 1:
            body = b'{"error":"simulated 500"}'
            self.send_response(500)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        if n == 2:
            # 模拟连接超时：迟迟不给响应头
            time.sleep(5)
            body = b'{"error":"late"}'
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        # 第 3 次：正常 SSE 流
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        for i in range(5):
            payload = json.dumps({"id": "chunk", "seq": i, "t": time.time()}, ensure_ascii=False).encode()
            line = b"data: " + payload + b"\n\n"
            self.wfile.write(b"%x\r\n%s\r\n" % (len(line), line))
            self.wfile.flush()
            time.sleep(0.4)
        self.wfile.write(b"0\r\n\r\n")
        self.wfile.flush()

if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 19001
    ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()
