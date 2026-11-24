"""Generate the icon mockup page and the final icon SVGs from grids.py."""

import subprocess
import sys
from pathlib import Path

from grids import BG, IDEAS, PAL, RECOMMENDED

HERE = Path(__file__).parent
OUT = HERE / "out"
REPO = HERE.parents[2]  # web/scripts/icons -> repo root
N = 16


def check(grid: list[str], name: str) -> None:
    assert len(grid) == N, (name, len(grid))
    for i, row in enumerate(grid):
        assert len(row) == N, (name, i, len(row), row)
        for ch in row:
            assert ch == "." or ch in PAL, (name, i, ch)


def rects(grid: list[str], cell: int, ox: int, oy: int, colors: dict[str, str]) -> str:
    """One <rect> per horizontal run of a colour, in whole device pixels."""
    out = []
    for y, row in enumerate(grid):
        x = 0
        while x < N:
            ch = row[x]
            if ch == ".":
                x += 1
                continue
            end = x
            while end < N and row[end] == ch:
                end += 1
            out.append(
                f'<rect x="{ox + x * cell}" y="{oy + y * cell}" '
                f'width="{(end - x) * cell}" height="{cell}" fill="{colors[ch]}"/>'
            )
            x = end
    return "".join(out)


def svg(grid: list[str], size: int, cell: int, *, bg: str | None = BG,
        colors: dict[str, str] = PAL, rx: int = 0, attrs: str = "") -> str:
    """Art at `cell` px per grid cell, centred on a `size` square."""
    off = (size - N * cell) // 2
    assert off * 2 + N * cell == size, (size, cell)
    back = f'<rect width="{size}" height="{size}" rx="{rx}" fill="{bg}"/>' if bg else ""
    return (
        f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {size} {size}" '
        f'width="{size}" height="{size}" shape-rendering="crispEdges"{attrs}>'
        f"{back}{rects(grid, cell, off, off, colors)}</svg>"
    )


def page_svg(grid: list[str], px: int) -> str:
    """Mockup display: 16-unit viewBox scaled by CSS to px."""
    return (
        f'<svg viewBox="0 0 16 16" width="{px}" height="{px}" shape-rendering="crispEdges" '
        f'aria-hidden="true"><rect width="16" height="16" fill="{BG}"/>'
        f"{rects(grid, 1, 0, 0, PAL)}</svg>"
    )


def maskable_svg(grid: list[str], px: int) -> str:
    """Mockup display of the maskable composition: art at 70% inside the square."""
    # 16 art units in a 24-unit square, the same proportion as icon-maskable-192.
    return (
        f'<svg viewBox="-4 -4 24 24" width="{px}" height="{px}" '
        f'shape-rendering="crispEdges" aria-hidden="true">'
        f'<rect x="-4" y="-4" width="24" height="24" fill="{BG}"/>'
        f"{rects(grid, 1, 0, 0, PAL)}</svg>"
    )


CSS = """
:root{--bg:#000;--fg:#f5f5f7;--mute:#8e8e93;--card:#1c1c1e;--line:#2c2c2e;--lime:#e6fb04}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);
 font:15px/1.45 -apple-system,BlinkMacSystemFont,"Inter","Segoe UI",Roboto,sans-serif}
main{max-width:1180px;margin:0 auto;padding:32px 16px 64px}
h1{font-size:28px;margin:0 0 4px;letter-spacing:-.01em}
p.lede{color:var(--mute);margin:0 0 28px;max-width:70ch}
.idea{background:var(--card);border:1px solid var(--line);border-radius:20px;padding:20px;margin:0 0 24px}
.idea.rec{border-color:var(--lime);box-shadow:0 0 0 1px var(--lime)}
.head{display:flex;gap:12px;align-items:baseline;flex-wrap:wrap;margin-bottom:4px}
.head h2{font-size:20px;margin:0}
.badge{background:var(--lime);color:#000;font-weight:600;font-size:12px;border-radius:99px;padding:2px 10px}
.why{color:var(--lime);margin:4px 0 0}
.blurb{color:var(--mute);margin:0 0 16px}
.row{display:flex;flex-wrap:wrap;gap:20px;align-items:flex-end}
figure{margin:0;display:flex;flex-direction:column;align-items:center;gap:6px}
figcaption{color:var(--mute);font-size:12px}
.big svg{max-width:100%;height:auto}
.scroll{overflow-x:auto;max-width:100%}
.home{width:300px;border-radius:28px;padding:22px 18px;
 background:linear-gradient(160deg,#2b4a6f 0%,#6d4b7d 55%,#c9796b 100%);
 display:grid;grid-template-columns:repeat(4,1fr);gap:16px 8px}
.home .app{display:flex;flex-direction:column;align-items:center;gap:5px;
 font-size:11px;color:#fff;text-shadow:0 1px 2px rgba(0,0,0,.5)}
.home .ic{width:48px;height:48px;overflow:hidden}
.home .ic svg{display:block;width:100%;height:100%}
.home .circle .ic{border-radius:50%}
.home .squircle .ic{border-radius:30%}
.home .other .ic{background:rgba(255,255,255,.25)}
.home.circle .other .ic{border-radius:50%}
.home.squircle .other .ic{border-radius:30%}
.light{background:#f2f2f7;border-radius:16px;padding:16px;display:flex;gap:18px;align-items:center;color:#1c1c1e}
.light .tab{display:flex;align-items:center;gap:8px;background:#fff;border-radius:10px;
 padding:6px 12px;font-size:13px;box-shadow:0 1px 2px rgba(0,0,0,.08)}
.light .r{border-radius:22%;overflow:hidden;display:block}
.light .r svg{display:block}
.group{margin-top:16px}
.group h3{font-size:13px;font-weight:600;color:var(--mute);margin:0 0 8px;text-transform:uppercase;letter-spacing:.04em}
"""


def home(grid: list[str], shape: str) -> str:
    cells = []
    labels = ["Clock", "Maps", "Camera", "Mail", "Notes", "", "Music", "Files"]
    for i, label in enumerate(labels):
        if label == "":
            cells.append(
                f'<div class="app {shape}"><div class="ic">{maskable_svg(grid, 48)}</div>herdr-pocket</div>'
            )
        else:
            cells.append(f'<div class="app other"><div class="ic"></div>{label}</div>')
    return f'<div class="home {shape}">{"".join(cells)}</div>'


def idea_html(idea: dict) -> str:
    g = idea["grid"]
    rec = idea["id"] == RECOMMENDED
    badge = '<span class="badge">Recommended</span>' if rec else ""
    why = (
        '<p class="why">Why: a lamb from the herd, carried in your pocket, reads at a glance; the lime pocket '
        "stays a clear shape at 32 px, and the lamb is the cute part.</p>"
        if rec else ""
    )
    sizes = "".join(
        f'<figure class="{"big" if s == 512 else ""}"><div class="{"scroll" if s == 512 else ""}">{page_svg(g, s)}</div><figcaption>{s} px</figcaption></figure>'
        for s in (512, 192, 48, 32)
    )
    light = (
        f'<div class="light">'
        f'<span class="r">{page_svg(g, 64)}</span>'
        f'<span class="r">{page_svg(g, 48)}</span>'
        f'<span class="tab">{page_svg(g, 16)} Agents</span>'
        f'<span class="tab">{page_svg(g, 32)} herdr-pocket</span>'
        f"</div>"
    )
    return f"""
<section class="idea{' rec' if rec else ''}" id="{idea['id']}">
 <div class="head"><h2>{idea['name']}</h2>{badge}</div>
 <p class="blurb">{idea['blurb']}</p>{why}
 <div class="row">{sizes}</div>
 <div class="row group">
  <div><h3>Android, circle mask</h3>{home(g, 'circle')}</div>
  <div><h3>Android, squircle mask</h3>{home(g, 'squircle')}</div>
 </div>
 <div class="group"><h3>Light background</h3>{light}</div>
</section>"""


def mockup() -> str:
    body = "".join(idea_html(i) for i in IDEAS)
    return f"""<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Icon ideas</title>
<style>{CSS}</style>
</head>
<body>
<main>
<h1>herdr-pocket icon ideas</h1>
<p class="lede">Six pixel-art ideas, each drawn on a 16&times;16 grid with the app's palette
(black, #f5f5f7, lime #e6fb04, orange #ff9f0a). Each is shown at 512, 192, 48 and 32 px,
on a fake Android home screen (maskable art, circle and squircle masks) and on a light
background. The home screens use the maskable layout: the art inside the central 80%.</p>
{body}
</main>
</body>
</html>
"""


def render(svg_text: str, dest: Path, size: int) -> None:
    src = OUT / (dest.stem + ".src.svg")
    src.write_text(svg_text)
    subprocess.run(["resvg", "-w", str(size), "-h", str(size), str(src), str(dest)], check=True)


def main() -> None:
    OUT.mkdir(exist_ok=True)
    for idea in IDEAS:
        check(idea["grid"], idea["id"])
        if "mono" in idea:
            check(idea["mono"], idea["id"] + "-mono")
    mode = sys.argv[1] if len(sys.argv) > 1 else "preview"
    if mode == "preview":
        for idea in IDEAS:
            render(svg(idea["grid"], 512, 32), OUT / f"prev-{idea['id']}.png", 512)
            render(svg(idea["grid"], 32, 2), OUT / f"prev32-{idea['id']}.png", 32)
        (OUT / "icon-ideas.html").write_text(mockup())
        return
    if mode == "mockup":
        (OUT / "icon-ideas.html").write_text(mockup())
        return
    if mode == "final":
        pub = REPO / "web/public"
        idea = next(i for i in IDEAS if i["id"] == RECOMMENDED)
        g = idea["grid"]
        # Source art: 16-unit grid, full-bleed background.
        src = (
            '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" shape-rendering="crispEdges">'
            f'<rect width="16" height="16" fill="{BG}"/>{rects(g, 1, 0, 0, PAL)}</svg>\n'
        )
        (pub / "icon.svg").write_text(src)
        fav = idea.get("favicon", g)
        (pub / "favicon.svg").write_text(
            '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" shape-rendering="crispEdges">'
            f'<rect width="16" height="16" rx="3" fill="{BG}"/>{rects(fav, 1, 0, 0, PAL)}</svg>\n'
        )
        # "any": full-bleed, whole-pixel cells (every size is a multiple of 16).
        for s in (144, 192, 256, 384, 512):
            render(svg(g, s, s // N), pub / f"icon-{s}.png", s)
        # maskable: art inside the 80% safe-zone circle; background fills the square.
        render(svg(g, 512, 22), pub / "icon-maskable-512.png", 512)
        render(svg(g, 192, 8), pub / "icon-maskable-192.png", 192)
        # apple-touch: opaque, a little padding for iOS's corner rounding.
        render(svg(g, 180, 10), OUT / "apple-touch-icon.png", 180)
        subprocess.run(
            ["magick", str(OUT / "apple-touch-icon.png"), "-background", BG, "-alpha", "remove",
             "-alpha", "off", str(pub / "apple-touch-icon.png")],
            check=True,
        )
        # monochrome: white silhouette on transparent, padded like maskable.
        white = {k: "#ffffff" for k in PAL}
        render(svg(idea["mono"], 512, 22, bg=None, colors=white), pub / "icon-monochrome-512.png", 512)
        return
    raise SystemExit(f"unknown mode {mode}")


if __name__ == "__main__":
    main()
