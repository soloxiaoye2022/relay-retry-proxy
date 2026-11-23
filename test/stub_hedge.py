import json, sys, time, threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
N = {"n": 0}
L = threading.Lock()

class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *a): pass
    def do_POST(self):
        ln = int(self.headers.get("Content-Length") or 0)
        if ln: self.rfile.read(ln)
        with L:
            N["n"] += 1; n = N["n"]
        print(f"[stub] #{n} start", flush=True)
        if n == 1:
            time.sleep(120)   # 永不返回响应头，模拟上游卡死
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        for i in range(3):
            p = json.dumps({"seq": i, "served_by": f"attempt#{n}"}).encode()
            line = b"data: " + p + b"\n\n"
            self.wfile.write(b"%x\r\n%s\r\n" % (len(line), line)); self.wfile.flush()
            time.sleep(0.3)
        self.wfile.write(b"0\r\n\r\n"); self.wfile.flush()
        print(f"[stub] #{n} done", flush=True)

ThreadingHTTPServer(("127.0.0.1", 19002), H).serve_forever()
