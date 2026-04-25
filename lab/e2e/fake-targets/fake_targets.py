import http.server
import os
import select
import socket
import socketserver
import ssl
import threading
import time
import urllib.parse


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

    def do_POST(self):
        if self.path != "/upload":
            self._write(404, b"not found\n")
            return

        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        if len(body) != length:
            self._write(400, b"incomplete upload\n")
            return
        if self.headers.get("Transfer-Encoding"):
            self._write(400, b"chunked upload not expected\n")
            return
        if self.headers.get("Content-Type") != "application/octet-stream":
            self._write(415, b"unexpected content type\n")
            return
        self._write(200, f"uploaded {len(body)}\n".encode("ascii"))

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

        headers = {}
        while True:
            header = self.rfile.readline()
            if header in (b"\r\n", b"\n", b""):
                break
            name, _, value = header.decode("iso-8859-1", "replace").partition(":")
            if name:
                headers[name.strip().lower()] = value.strip()

        if method in ("GET", "POST"):
            self._handle_http(method, target, headers)
            return

        if method != "CONNECT":
            self.wfile.write(b"HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\n\r\n")
            return

        if target == "fake-targets:9000":
            self.wfile.write(b"HTTP/1.1 200 Connection Established\r\nContent-Length: 0\r\n\r\n")
            return
        if target == "fake-targets:9443":
            self._tunnel(("127.0.0.1", 9443))
            return

        self.wfile.write(b"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")

    def _handle_http(self, method, target, headers):
        parsed = urllib.parse.urlparse(target)
        if parsed.scheme != "http" or parsed.netloc != "fake-targets:8080":
            self.wfile.write(b"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
            return

        path = parsed.path or "/"
        if parsed.query:
            path = path + "?" + parsed.query

        with socket.create_connection(("127.0.0.1", 8080), timeout=2) as upstream:
            content_length = 0
            if method == "POST":
                content_length = int(headers.get("content-length", "0"))
                body = self.rfile.read(content_length)
            else:
                body = b""

            upstream.sendall(
                f"{method} {path} HTTP/1.1\r\n"
                "Host: fake-targets:8080\r\n"
                "Connection: close\r\n"
                f"Content-Length: {content_length}\r\n"
                "Content-Type: application/octet-stream\r\n"
                "\r\n"
                .encode("ascii")
            )
            if body:
                upstream.sendall(body)
            while True:
                chunk = upstream.recv(4096)
                if not chunk:
                    break
                self.wfile.write(chunk)

    def _tunnel(self, upstream_addr):
        try:
            upstream = socket.create_connection(upstream_addr, timeout=2)
        except OSError:
            self.wfile.write(b"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
            return

        self.wfile.write(b"HTTP/1.1 200 Connection Established\r\n\r\n")
        self.wfile.flush()

        sockets = [self.connection, upstream]
        try:
            while True:
                readable, _, _ = select.select(sockets, [], [], 10)
                if not readable:
                    return
                for src in readable:
                    dst = upstream if src is self.connection else self.connection
                    data = src.recv(4096)
                    if not data:
                        return
                    dst.sendall(data)
        finally:
            upstream.close()


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

def serve_tls():
    socketserver.ThreadingTCPServer.allow_reuse_address = True
    cert_path = os.path.join(os.path.dirname(__file__), "fake-targets.crt")
    key_path = os.path.join(os.path.dirname(__file__), "fake-targets.key")
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(certfile=cert_path, keyfile=key_path)
    with socketserver.ThreadingTCPServer(("0.0.0.0", 9443), HTTPHandler) as server:
        server.socket = context.wrap_socket(server.socket, server_side=True)
        server.serve_forever()

for fn in (serve_http, serve_proxy, serve_tcp, serve_tls):
    thread = threading.Thread(target=fn, daemon=True)
    thread.start()

while True:
    time.sleep(3600)
