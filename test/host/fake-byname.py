#!/usr/bin/python3
"""A stand-in byname binary for the host test: just enough of `version`,
`snapshot` and `serve` for install.sh, restore.sh and byname-verify.

run.sh makes one per release, filling in the three values below. Like the real
binary it migrates the database on start and refuses a schema newer than its own.
"""
import datetime
import http.server
import json
import os
import shutil
import sqlite3
import sys

VERSION = "@VERSION@"
SCHEMA = int("@SCHEMA@")  # the schema version this release migrates to
BROKEN = "@BROKEN@" == "1"  # serve migrates, then exits: a release that fails its health check


def die(msg):
    print(f"byname: {msg}", file=sys.stderr)
    sys.exit(1)


def open_db(path):
    con = sqlite3.connect(path, isolation_level=None)
    v = con.execute("PRAGMA user_version").fetchone()[0]
    if v > SCHEMA:
        die(f"database schema version {v} is newer than this binary ({SCHEMA}); refusing to run")
    if v < SCHEMA:
        con.execute(f"PRAGMA user_version = {SCHEMA}")
    return con


def flags(args):
    out, i = {}, 0
    while i < len(args):
        k = args[i].lstrip("-")
        if "=" in k:
            k, v = k.split("=", 1)
            i += 1
        else:
            v = args[i + 1]
            i += 2
        out[k] = v
    return out


def snapshot(args):
    f = flags(args)
    db = f.get("db", "/var/lib/byname/byname.db")
    d = f.get("snapshot-dir", "/var/lib/byname/snapshots")
    keep = int(f.get("snapshot-keep", "14"))
    con = open_db(db)
    now = datetime.datetime.now(datetime.timezone.utc)
    path = os.path.join(d, now.strftime("byname-%Y%m%dT%H%M%S.") + f"{now.microsecond // 1000:03d}Z.db")
    con.execute("VACUUM INTO ?", (path,))
    con.close()
    names = sorted(n for n in os.listdir(d) if n.startswith("byname-") and n.endswith(".db"))
    for n in names[: max(len(names) - keep, 0)]:
        os.remove(os.path.join(d, n))
    print(path)


class Handler(http.server.BaseHTTPRequestHandler):
    hosts = []

    def do_GET(self):
        host = (self.headers.get("Host") or "").lower()
        if not host.startswith("["):
            host = host.split(":")[0]
        if host not in self.hosts:
            return self.reply(421, {"error": "unknown host"})
        if self.path == "/_/healthz":
            return self.reply(200, {"status": "ok", "version": VERSION, "low_disk": False})
        self.reply(404, {"error": "not found"})

    def reply(self, code, body):
        data = json.dumps(body, indent=2).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *args):
        pass


def serve():
    db = os.environ.get("BYNAME_DB", "/var/lib/byname/byname.db")
    open_db(db).close()
    if os.geteuid() == 0:  # systemd runs the real one as User=byname
        shutil.chown(db, "byname", "byname")
    if BROKEN:
        die(f"{VERSION} is broken on purpose (host test)")
    Handler.hosts = [h.strip().lower() for h in os.environ.get("BYNAME_HOSTS", "").split(",") if h.strip()]
    addr, port = os.environ.get("BYNAME_LISTEN", ":80").rsplit(":", 1)
    http.server.ThreadingHTTPServer((addr or "0.0.0.0", int(port)), Handler).serve_forever()


def main():
    cmd, args = (sys.argv[1], sys.argv[2:]) if len(sys.argv) > 1 else ("", [])
    if cmd == "version":
        print(f"byname {VERSION}")
    elif cmd == "snapshot":
        snapshot(args)
    elif cmd == "serve":
        serve()
    else:
        die(f"fake byname: unsupported command {cmd!r}")


main()
