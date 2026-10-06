#!/usr/bin/env python3
"""Generate the README's light/dark supervisor overview with the Python standard library."""
from pathlib import Path

OUT = Path(__file__).resolve().parents[1] / "docs" / "architecture" / "generated"


def render(theme: str) -> str:
    parts = ['''<svg xmlns="http://www.w3.org/2000/svg" width="1120" height="520" viewBox="0 0 1120 520" role="img" aria-labelledby="title desc">
<title id="title">GPU Workload Supervisor overview</title>
<desc id="desc">Clients send requests through the execution proxy to catalog-configured workload services sharing one GPU. The separate controller stops and verifies workloads before starting the next. Proxy and controller share durable SQLite state for admission, leases, and transition journaling. Blue components belong to this repository.</desc>
<defs><marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M 0 0 L 10 5 L 0 10 z" fill="#64748b"/></marker></defs>
<rect width="1120" height="520" rx="20" fill="#f8fafc"/>
<g font-family="Arial, Helvetica, sans-serif">
<text x="32" y="65" fill="#475569" font-size="14">Requests pass through the proxy. The controller switches workloads separately.</text>''']
    nodes = [
        (32, 111, "Clients", ["Apps and tools"], False, False),
        (310, 111, "Execution proxy", ["gpu-workload-proxy", "Admit and forward"], True, True),
        (588, 111, "Text / media services", ["One active at a time", "Runtimes you configure"], False, False),
        (866, 111, "Shared GPU", ["One device", "Selected workload"], False, False),
        (310, 332, "Durable state", ["SQLite", "Leases and journal"], True, False),
        (588, 332, "Controller", ["gpu-mode", "Stop · verify · start"], True, True),
    ]
    for x, y, label, lines, owned, badge in nodes:
        fill, stroke, ink = ("#dbeafe", "#1d4ed8", "#1e3a8a") if owned else ("#eef2f6", "#94a3b8", "#334155")
        parts.append(f'<rect x="{x}" y="{y}" width="222" height="141" rx="12" fill="{fill}" stroke="{stroke}" stroke-width="{3 if owned else 1.5}"/>')
        if badge:
            parts.append(f'<rect x="{x+18}" y="{y-14}" width="132" height="25" rx="12" fill="#1d4ed8"/><text x="{x+84}" y="{y+3}" text-anchor="middle" font-size="12" font-weight="700" fill="#ffffff">This repository</text>')
        size = 18 if label == "Text / media services" else 20
        parts.append(f'<text x="{x+18}" y="{y+41}" font-size="{size}" font-weight="700" fill="{ink}">{label}</text>')
        for i, line in enumerate(lines):
            parts.append(f'<text x="{x+18}" y="{y+77+i*23}" font-size="15" fill="{ink}">{line}</text>')
    for x in [260, 538, 816]:
        parts.append(f'<path d="M {x} 180 H {x+41}" fill="none" stroke="#64748b" stroke-width="2" marker-end="url(#arrow)"/>')
    parts.extend([
        '<path d="M 421 258 V 323" fill="none" stroke="#64748b" stroke-width="2" marker-end="url(#arrow)"/>',
        '<text x="438" y="292" font-size="12" fill="#475569">Admit / track</text>',
        '<path d="M 699 312 V 261" fill="none" stroke="#64748b" stroke-width="2" marker-end="url(#arrow)"/>',
        '<text x="716" y="292" font-size="12" fill="#475569">Switch / verify</text>',
        '<path d="M 582 403 H 541" fill="none" stroke="#64748b" stroke-width="2" marker-end="url(#arrow)"/>',
        '<text x="560" y="386" text-anchor="middle" font-size="11" fill="#475569">State</text>',
        '<text x="32" y="504" font-size="14" fill="#475569">Blue: supervisor components. Gray: clients, runtime services, and hardware supplied by the deployment.</text>',
        '</g></svg>\n',
    ])
    svg = "\n".join(parts)
    if theme == "dark":
        colors = {
            "#f8fafc": "#0d1117", "#0f172a": "#f0f6fc", "#475569": "#a6b0bd",
            "#eef2f6": "#161b22", "#94a3b8": "#6e7681", "#334155": "#c9d1d9",
            "#64748b": "#8b949e", "#dbeafe": "#122a4f", "#1d4ed8": "#388bfd",
            "#1e3a8a": "#cae3ff", "#ffffff": "#0d1117",
        }
        for light, dark in colors.items():
            svg = svg.replace(light, dark)
    return svg


if __name__ == "__main__":
    OUT.mkdir(parents=True, exist_ok=True)
    for theme in ("light", "dark"):
        (OUT / f"overview-{theme}.svg").write_text(render(theme), encoding="utf-8")
