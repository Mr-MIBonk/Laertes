"""Packs the PNGs rendered by render.html (headless Edge --dump-dom output on stdin) into
laertes.ico. ICO entries may hold PNG data as is (Windows Vista and later)."""
import base64
import json
import re
import struct
import sys

dom = sys.stdin.read()
pngs = {int(n): base64.b64decode(b) for n, b in json.loads(re.search(r'ICONS(.*?)ICONS', dom, re.S).group(1)).items()}
sizes = sorted(pngs)
header = struct.pack('<HHH', 0, 1, len(sizes))
offset = len(header) + 16 * len(sizes)
entries, data = b'', b''
for n in sizes:
    png = pngs[n]
    # width/height 0 means 256; 1 colour plane, 32 bits per pixel
    entries += struct.pack('<BBBBHHII', n % 256, n % 256, 0, 0, 1, 32, len(png), offset + len(data))
    data += png
with open(sys.argv[1], 'wb') as f:
    f.write(header + entries + data)
print(f'{sys.argv[1]}: {len(sizes)} sizes {sizes}')
