#!/usr/bin/env python3
"""Render captured tmux terminal cells as compact PNGs (text is unmodified)."""
import html, pathlib, subprocess, sys
src, dst = map(pathlib.Path, sys.argv[1:])
lines = src.read_text(errors='replace').splitlines()
cols = max((len(x) for x in lines), default=1)
width, line_h, pad = cols * 8.2 + 32, 18, 20
height = max(1, len(lines)) * line_h + pad * 2
parts = [f'<svg xmlns="http://www.w3.org/2000/svg" width="{width:.0f}" height="{height:.0f}" viewBox="0 0 {width:.0f} {height:.0f}">',
         f'<rect width="100%" height="100%" rx="12" fill="#111318"/>',
         f'<text x="{pad}" y="{pad + 13}" fill="#e8e9ed" font-family="Adwaita Mono,DejaVu Sans Mono,monospace" font-size="13">']
for i, line in enumerate(lines):
    if i:
        parts.append(f'<tspan x="{pad}" dy="{line_h}">')
    parts.append(html.escape(line))
    if i:
        parts.append('</tspan>')
parts.append('</text></svg>')
svg = dst.with_suffix('.svg')
svg.write_text(''.join(parts))
subprocess.run(['rsvg-convert', '-o', str(dst), str(svg)], check=True)
svg.unlink()
