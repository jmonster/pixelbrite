#!/usr/bin/env python3
"""A Pixel Brite adapter in Python, using only the standard library.

It serves test tones through status, search and live audio. See
pkg/adapter/README.md for the spec.
"""

import argparse
import array
import json
import math
import socketserver
import struct
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler
from urllib.parse import parse_qs, unquote, urlsplit

RATE = 48000
TRACK_MS = 20000
NOTES = {"c4": ("C4", 261.63), "e4": ("E4", 329.63), "g4": ("G4", 392.00), "a4": ("A4", 440.00), "c5": ("C5", 523.25)}


def track(note_id):
    name, hz = NOTES[note_id]
    return {"kind": "track", "id": note_id, "title": f"{name} ({hz:g} Hz)", "artist": "Test tones", "duration_ms": TRACK_MS}


def record(kind, payload):
    """A live audio record: type byte, uint32 little-endian length, payload."""
    return kind + struct.pack("<I", len(payload)) + payload


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print(fmt % args, file=sys.stderr)  # the default needs a TCP client address

    def send_json(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        url = urlsplit(self.path)
        query = parse_qs(url.query)
        if url.path == "/status":
            if "after" in query:
                time.sleep(25)  # the status never changes
            self.send_json(200, {"revision": 1, "status": {"name": "Python tones", "ready": True}})
        elif url.path == "/search":
            q = query.get("q", [""])[0].lower()
            self.send_json(200, {"items": [track(n) for n in NOTES if q and q in track(n)["title"].lower()]})
        elif url.path.startswith("/play/"):
            self.play(unquote(url.path[len("/play/"):]), int(query.get("start_ms", ["0"])[0]))
        else:
            self.send_json(404, {"message": "not found"})

    def do_POST(self):
        self.send_json(404, {"message": "this adapter has no setup steps"})

    def do_DELETE(self):
        if self.path == "/member":
            self.send_response(204)
            self.end_headers()
        else:
            self.send_json(404, {"message": "this adapter has no setup steps"})

    def play(self, note_id, start_ms):
        if note_id not in NOTES:
            self.send_json(404, {"message": "no such track"})
            return
        hz = NOTES[note_id][1]
        self.send_response(200)
        self.send_header("Sample-Rate", str(RATE))
        self.end_headers()
        frame, end = max(start_ms, 0) * RATE // 1000, TRACK_MS * RATE // 1000
        try:
            while frame < end:
                count = min(RATE // 10, end - frame)
                samples = array.array("f")
                for i in range(frame, frame + count):
                    v = 0.2 * math.sin(2 * math.pi * hz * i / RATE)
                    samples.extend((v, v))  # left, right
                if sys.byteorder == "big":
                    samples.byteswap()
                self.wfile.write(record(b"a", samples.tobytes()))
                frame += count
            self.wfile.write(record(b"e", b"{}"))
        except (BrokenPipeError, ConnectionResetError):
            pass  # the server stopped the track


class Server(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("-listen", required=True)
    parser.add_argument("-data", required=True)
    parser.add_argument("-capabilities", required=True)
    args = parser.parse_args()
    for chosen in args.capabilities.split(","):
        name, _, version = chosen.partition("=")
        if version != "1":
            print(f"incompatible {name} version {version}", flush=True)
            return
    server = Server(args.listen, Handler)

    def stop_when_stdin_closes():
        sys.stdin.buffer.read()
        server.shutdown()

    threading.Thread(target=stop_when_stdin_closes, daemon=True).start()
    print("ready", flush=True)  # stdout is buffered when it isn't a terminal
    server.serve_forever()


if __name__ == "__main__":
    main()
