import json, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self,*a): pass
    def do_POST(self):
        ln=int(self.headers.get("Content-Length") or 0)
        if ln: self.rfile.read(ln)
        self.send_response(200)
        self.send_header("Content-Type","text/event-stream")
        self.send_header("Transfer-Encoding","chunked")
        self.end_headers()
        def send(obj):
            line=b"data: "+json.dumps(obj).encode()+b"\n\n"
            self.wfile.write(b"%x\r\n%s\r\n"%(len(line),line)); self.wfile.flush()
        for i in range(4):
            send({"choices":[{"delta":{"content":"token%d "%i}}]})
            time.sleep(0.25)
        # 末尾带 usage（OpenAI 风格）
        send({"choices":[],"usage":{"prompt_tokens":1234,"completion_tokens":567,"total_tokens":1801}})
        self.wfile.write(b"0\r\n\r\n"); self.wfile.flush()

ThreadingHTTPServer(("127.0.0.1",19003),H).serve_forever()
