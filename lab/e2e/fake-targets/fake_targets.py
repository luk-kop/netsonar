import http.server
import socket
import socketserver
import threading
import time


class HTTPHandler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/ok":
            self._write(200, b"healthy\n")
            return
        if self.path == "/unhealthy":
            self._write(200, b"not-ok\n")
            return
        if self.path == "/error":
            self._write(500, b"error\n")
            return
        if self.path == "/error-with-healthy-body":
            self._write(500, b"healthy\n")
            return
        self._write(404, b"not found\n")

    def log_message(self, fmt, *args):
        return

    def _write(self, status, body):
        self.send_response(status)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


class ProxyHandler(socketserver.StreamRequestHandler):
    def handle(self):
        line = self.rfile.readline().decode("ascii", "replace").strip()
        if not line:
            return

        parts = line.split()
        method = parts[0] if len(parts) >= 1 else ""
        target = parts[1] if len(parts) >= 2 else ""

        while True:
            header = self.rfile.readline()
            if header in (b"\r\n", b"\n", b""):
                break

        if method != "CONNECT":
            self.wfile.write(b"HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\n\r\n")
            return

        if target == "fake-targets:9000":
            self.wfile.write(b"HTTP/1.1 200 Connection Established\r\nContent-Length: 0\r\n\r\n")
            return

        self.wfile.write(b"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")


def serve_http():
    socketserver.ThreadingTCPServer.allow_reuse_address = True
    with socketserver.ThreadingTCPServer(("0.0.0.0", 8080), HTTPHandler) as server:
        server.serve_forever()


def serve_proxy():
    with socketserver.ThreadingTCPServer(("0.0.0.0", 8888), ProxyHandler) as server:
        server.serve_forever()


def serve_tcp():
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
        listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        listener.bind(("0.0.0.0", 9000))
        listener.listen()
        while True:
            conn, _ = listener.accept()
            conn.close()


for fn in (serve_http, serve_proxy, serve_tcp):
    thread = threading.Thread(target=fn, daemon=True)
    thread.start()

while True:
    time.sleep(3600)
