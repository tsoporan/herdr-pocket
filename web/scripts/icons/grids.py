"""Pixel-art grids for the herdr-pocket icon ideas. 16x16, '.' = background."""

BG = "#000000"

# The app's palette.
PAL = {
    "W": "#f5f5f7",  # text / wool
    "L": "#e6fb04",  # accent lime
    "D": "#3d4300",  # dark olive (accent on dark)
    "O": "#ff9f0a",  # needs-you orange
    "o": "#ffb340",  # light orange
    "G": "#8e8e93",  # secondary grey
    "g": "#48484a",  # dark grey
    "S": "#2c2c2e",  # surface
    "s": "#1c1c1e",  # surface low
    "B": "#3a3a3c",  # border grey
    "N": "#30d158",  # green status
    "C": "#64d2ff",  # cyan
    "K": "#000000",  # black on top of a colour
    "h": "#636366",  # mid-dark grey (sheep face)
    "P": "#ff7a90",  # tongue pink
}


def _stamp(canvas: list[list[str]], sprite: list[str], x: int, y: int) -> None:
    for dy, row in enumerate(sprite):
        for dx, ch in enumerate(row):
            if ch != "." and 0 <= x + dx < 16 and 0 <= y + dy < 16:
                canvas[y + dy][x + dx] = ch


def _herd() -> list[str]:
    lamb = [
        ".WWWW.",
        "WWWWWW",
        "GhhhhG",
        ".WhhW.",
        ".hhhh.",
    ]
    c = [["."] * 16 for _ in range(16)]
    _stamp(c, lamb, 0, 6)
    _stamp(c, lamb, 10, 6)
    _stamp(c, lamb, 5, 2)
    pocket = [
        ".LLLLLLLLLLLLLL.",
        ".LLLLLLLLLLLLLL.",
        ".LDLDLDLDLDLDLL.",
        ".LLLLLLLLLLLLLL.",
        "..LLLLLLLLLLLL..",
    ]
    _stamp(c, pocket, 0, 10)
    return ["".join(r) for r in c]


IDEAS = [
    {
        "id": "pocket-sheep",
        "name": "Pocket sheep",
        "blurb": "A lamb peeking over the rim of a lime pocket, front hooves hooked on the edge.",
        "grid": [
            "................",
            "....WW.WW.WW....",
            "...WWWWWWWWWW...",
            "..WWWWWWWWWWWW..",
            "..WWWWWWWWWWWW..",
            ".GGWhhhhhhhhWGG.",
            ".GG.hhhhhhhh.GG.",
            "....hhWhhWhh....",
            "....OhWhhWhO....",
            ".....hhhhhh.....",
            ".LLWWLLLLLLWWLL.",
            ".LLLLLLLLLLLLLL.",
            ".LDLDLDLDLDLDLL.",
            ".LLLLLLLLLLLLLL.",
            "..LLLLLLLLLLLL..",
            "................",
        ],
        # Monochrome: wool and pocket white, face and stitches cut out.
        "mono": [
            "................",
            "....WW.WW.WW....",
            "...WWWWWWWWWW...",
            "..WWWWWWWWWWWW..",
            "..WWWWWWWWWWWW..",
            ".WW..........WW.",
            ".WW.WWWWWWWW.WW.",
            "....WW.WW.WW....",
            "....WW.WW.WW....",
            ".....WWWWWW.....",
            "................",
            ".WWWWWWWWWWWWWW.",
            ".W.W.W.W.W.W.WW.",
            ".WWWWWWWWWWWWWW.",
            "..WWWWWWWWWWWW..",
            "................",
        ],
    },
    {
        "id": "prompt-pocket",
        "name": "Prompt pocket",
        "blurb": "A little terminal card tucked in a lime pocket, showing a prompt and a block cursor.",
        "grid": [
            "................",
            "................",
            "...BBBBBBBBBB...",
            "...BssssssssB...",
            "...BsLssssssB...",
            "...BssLsssssB...",
            "...BsssLssssB...",
            "...BssLssWWsB...",
            "...BsLsssWWsB...",
            "...BssssssssB...",
            ".LLLLLLLLLLLLLL.",
            ".LLLLLLLLLLLLLL.",
            ".LDLDLDLDLDLDLL.",
            ".LLLLLLLLLLLLLL.",
            "..LLLLLLLLLLLL..",
            "................",
        ],
    },
    {
        "id": "crook-lamb",
        "name": "Crook and lamb",
        "blurb": "A lime shepherd's crook with a small lamb trotting beside it.",
        "grid": [
            "................",
            "......LLLLL.....",
            ".....LLLLLLL....",
            ".....LL...LL....",
            ".....LL...LL....",
            "..........LL....",
            "..........LL....",
            "..........LL....",
            "..WWWW....LL....",
            ".WWWWWWGG.LL....",
            "WWWWWWWGWG.L....",
            ".WWWWWWGGG.L....",
            "..WWWWW....L....",
            "..G..G.....L....",
            "..G..G.....L....",
            "................",
        ],
    },
    {
        "id": "herd",
        "name": "Herd in a pocket",
        "blurb": "Three marshmallow sheep crowded into one pocket, the middle one standing tall.",
        "grid": _herd(),
    },
    {
        "id": "corgi",
        "name": "Herd dog",
        "blurb": "An orange herding corgi peeking out of a dark pocket with lime stitching.",
        "grid": [
            "................",
            ".OO..........OO.",
            ".OoO........OoO.",
            ".OooOOOOOOOOooO.",
            "..OOOOOOOOOOOO..",
            "..OOOOOWWOOOOO..",
            "..OKOOOWWOOOKO..",
            "..OOOOWWWWOOOO..",
            "..OOWWWKKWWWOO..",
            "....WWWPPWWW....",
            ".SSSSSSPPSSSSSS.",
            ".SSSSSSSSSSSSSS.",
            ".SLSLSLSLSLSLSS.",
            ".SSSSSSSSSSSSSS.",
            "..SSSSSSSSSSSS..",
            "................",
        ],
    },
    {
        "id": "term-sheep",
        "name": "Sheep in a shell",
        "blurb": "A terminal window with herdr's status lights, and a sheep sitting at the prompt.",
        "grid": [
            "................",
            "................",
            ".BBBBBBBBBBBBBB.",
            ".BONLBBBBBBBBBB.",
            ".BBBBBBBBBBBBBB.",
            ".BssssssssssssB.",
            ".BssssssssssssB.",
            ".BssWWWWssssssB.",
            ".BsWWWWWWGGsssB.",
            ".BsWWWWWWGWsssB.",
            ".BssWWWWWGGsssB.",
            ".BsssGssGsssLsB.",
            ".BssssssssssLsB.",
            ".BssssssssssssB.",
            ".BBBBBBBBBBBBBB.",
            "................",
        ],
    },
]

RECOMMENDED = "pocket-sheep"
