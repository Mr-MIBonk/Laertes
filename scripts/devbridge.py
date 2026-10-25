"""Development connection to the ODIS Jython bridge (mcpserver.py, port 8766).

While this script is connected, the bridge refuses Laertes's commands, so the two never drive
ODIS at once; Laertes works again as soon as the script exits.

    python scripts/devbridge.py '{"action": "ping"}' '{"action": "job_list"}'
    python scripts/devbridge.py            (one JSON command per line from stdin; empty line ends)

A "token": "@" in a command is replaced by the debug token (for _debug). If a command hangs
inside ODIS, its connection stays busy (and Laertes blocked) even after this script exits;
'{"action": "shutdown"}' still stops the bridge then, and Laertes starts it again.
"""
import json
import socket
import sys

PORT = 8766
TOKEN_FILE = r"C:\ProgramData\OE\macros\.mcpserver_debug_token"


def send(sock, reader, line):
    cmd = json.loads(line)
    if cmd.get("token") == "@":
        with open(TOKEN_FILE, encoding="utf-8") as f:
            cmd["token"] = f.read().strip()
    sock.sendall((json.dumps(cmd) + "\n").encode("utf-8"))
    reply = reader.readline()
    if not reply:
        raise SystemExit("the bridge closed the connection")
    print(json.dumps(json.loads(reply), indent=1, ensure_ascii=False), flush=True)


def main():
    sys.stdout.reconfigure(encoding="utf-8")
    try:
        sock = socket.create_connection(("127.0.0.1", PORT), 5)
    except OSError as e:
        raise SystemExit("ODIS bridge development port %d is not reachable: %s" % (PORT, e))
    sock.settimeout(300)  # some actions read from the control units
    reader = sock.makefile("r", encoding="utf-8")
    try:
        lines = sys.argv[1:] or iter(sys.stdin.readline, "")
        for line in lines:
            if not line.strip():
                if len(sys.argv) == 1:
                    break
                continue
            send(sock, reader, line)
    finally:
        sock.close()


if __name__ == "__main__":
    main()
