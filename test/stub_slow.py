import json, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self,*a): pass
    def do_POST(self):
        ln=int(self.headers.get("Content-Length") or 0)
        if ln: self.rfile.read(ln)
        time.sleep(2.0)                      # 模拟 prefill：2 秒后才吐第一个字节
        self.send_response(200)
        self.send_header("Content-Type","text/event-stream")
        self.send_header("Transfer-Encoding","chunked")
        self.end_headers()
        def send(o):
            line=b"data: "+json.dumps(o).encode()+b"\n\n"
            self.wfile.write(b"%x\r\n%s\r\n"%(len(line),line)); self.wfile.flush()
        for i in range(6):
            send({"choices":[{"delta":{"content":"片段%d "%i}}]})
            time.sleep(0.5)                  # 每秒约 2 个片段
        send({"choices":[],"usage":{
            "prompt_tokens":384000,"completion_tokens":600,"total_tokens":384600,
            "prompt_tokens_details":{"cached_tokens":100000},
            "completion_tokens_details":{"reasoning_tokens":120}}})
        self.wfile.write(b"0\r\n\r\n"); self.wfile.flush()

ThreadingHTTPServer(("127.0.0.1",19004),H).serve_forever()
