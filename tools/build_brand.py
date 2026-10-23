#!/usr/bin/env python3
"""Build the Panko SVG asset family and PNG/ICO exports; requires rsvg-convert."""
from pathlib import Path
import shutil
import struct
import subprocess

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / 'docs/brand'
SEGMENTS = ['512,218 611,276 595,360 374,299',
            '628,286 767,367 767,470 685,518 680,412 611,372',
            '767,491 767,653 666,714 589,662 680,606 685,539',
            '649,725 512,807 407,744 427,660 512,709 570,675',
            '391,735 257,653 257,548 343,498 343,606 411,646',
            '257,527 257,367 357,309 435,355 343,412 343,477']
NOTCHES = 'M512 263V320 M297 387L350 418 M297 631L350 600 M512 761V700 M727 631L674 600 M727 387L674 418'
CUBE = 'M343 415L512 315L680 415V606L512 706L343 606Z'
EDGES = 'M343 415L512 515L680 415 M512 515V706'


def mark(mode='color', small=False):
    mono = mode != 'color'
    ink = {'black': '#17212b', 'white': '#ffffff'}.get(mode)
    polygons = ''.join(f'<polygon points="{p}"/>' for p in SEGMENTS)
    defs = '''<defs><linearGradient id="orange" x1="0" y1="0" x2="1" y2="1">
<stop stop-color="#ffb000"/><stop offset="1" stop-color="#cd7100"/></linearGradient></defs>'''
    if mono:
        # Cut out the cube and radial slots, retaining genuine alpha in one-color marks.
        defs = f'''<defs><mask id="cut"><rect width="1024" height="1024" fill="white"/>
<path d="{CUBE}" fill="black" stroke="black" stroke-width="12"/>
<path d="{NOTCHES}" stroke="black" stroke-width="18"/></mask></defs>'''
        body = f'<g fill="{ink}" mask="url(#cut)">{polygons}</g>'
        body += f'<path d="{CUBE}" fill="none" stroke="{ink}" stroke-width="14"/>'
    else:
        body = f'<g fill="url(#orange)">{polygons}</g>'
        if not small:
            body += f'<path d="{NOTCHES}" fill="none" stroke="white" stroke-width="18"/>'
        body += f'<path d="{CUBE}" fill="white" stroke="#999999" stroke-width="12"/>'
    body += f'<path d="{EDGES}" fill="none" stroke="{ink if mono else "#999999"}" stroke-width="{18 if small else 12}" stroke-linejoin="round"/>'
    return defs + f'<g transform="translate(-212 -218)">{body}</g>'


def svg(name, width, height, body, label):
    content = f'''<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" viewBox="0 0 {width} {height}" role="img" aria-labelledby="title"><title id="title">{label}</title>{body}</svg>\n'''
    (OUT / f'{name}.svg').write_text(content)


def placed(mode, x, y, scale, small=False):
    return f'<g transform="translate({x} {y}) scale({scale})">{mark(mode, small)}</g>'


def text(x, y, size, fill, value, weight=700):
    return f'<text x="{x}" y="{y}" font-family="DejaVu Sans, sans-serif" font-size="{size}" font-weight="{weight}" letter-spacing="-1" fill="{fill}">{value}</text>'


def build():
    OUT.mkdir(parents=True, exist_ok=True)
    for mode in ('color', 'black', 'white'):
        svg(f'panko-mark-{mode}', 640, 640, placed(mode, 20, 25, 1), f'Panko {mode} mark')
    for theme, ink in [('light', '#17212b'), ('dark', '#ffffff')]:
        svg(f'panko-horizontal-{theme}', 740, 280,
            placed('color', 18, 16, .42) + text(300, 186, 132, ink, 'panko'),
            f'Panko wordmark for {theme} backgrounds')
        svg(f'panko-stacked-{theme}', 640, 790,
            placed('color', 20, 25, 1) + f'<g text-anchor="middle">{text(320, 736, 102, ink, "panko")}</g>',
            f'Panko stacked logo for {theme} backgrounds')
    for mode in ('black', 'white'):
        svg(f'panko-horizontal-{mode}', 740, 280,
            placed(mode, 18, 16, .42) + text(300, 186, 132, '#17212b' if mode == 'black' else '#ffffff', 'panko'),
            f'Panko one-color {mode} wordmark')
    svg('favicon', 64, 64, placed('color', 2, 2.5, .1, small=True), 'Panko icon')
    svg('panko-social', 1280, 640,
        '<rect width="1280" height="640" fill="#111b26"/>'
        '<path d="M0 624H1280" stroke="#f5a000" stroke-width="16"/>'
        + placed('color', 70, 145, .56)
        + text(460, 280, 122, '#ffffff', 'panko')
        + text(466, 355, 30, '#ffb000', 'A thin wrapper for Terraform.', 400)
        + text(466, 416, 22, '#d3dbe4', 'Open source. Built on OpenTofu and Kubernetes.', 400),
        'Panko — a thin wrapper for Terraform')
    converter = shutil.which('rsvg-convert')
    if not converter:
        raise SystemExit('SVGs written. Install rsvg-convert to build PNG and ICO exports.')
    for path in sorted(OUT.glob('*.svg')):
        subprocess.run([converter, str(path), '-o', str(path.with_suffix('.png'))], check=True)
    for size in (16, 32, 48, 180, 192, 512):
        source = 'favicon.svg' if size <= 48 else 'panko-mark-color.svg'
        subprocess.run([converter, str(OUT / source), '-w', str(size), '-h', str(size), '-o', str(OUT / f'panko-icon-{size}.png')], check=True)
    # Modern ICO files contain PNG payloads; no raster manipulation is required.
    sizes = (16, 32, 48)
    payloads = [(OUT / f'panko-icon-{s}.png').read_bytes() for s in sizes]
    offset = 6 + 16 * len(sizes)
    entries = []
    for size, data in zip(sizes, payloads):
        entries.append(struct.pack('<BBBBHHII', size, size, 0, 0, 1, 32, len(data), offset))
        offset += len(data)
    (OUT / 'favicon.ico').write_bytes(struct.pack('<HHH', 0, 1, len(sizes)) + b''.join(entries) + b''.join(payloads))


if __name__ == '__main__':
    build()
