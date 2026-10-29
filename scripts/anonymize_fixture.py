#!/usr/bin/env python3
"""Strip third-party personal data from Google Maps fixture files, in place.

Google place payloads embed reviews: author name, Google account id, profile
and avatar URLs, review text, owner replies. None of it is needed to test the
parsers, and these files are committed to a repository that may be public.

Replaced: author names, avatar URLs, review and reply texts, Google account
ids (21-digit contributor / owner ids, everywhere they appear).
Kept: structure, business data (name, address, phone...), ratings, dates.

Idempotent. Usage: scripts/anonymize_fixture.py path/to/fixture.json [...]
"""
import json
import re
import sys

ID_RE = re.compile(r"maps/contrib/(\d{15,25})")
FAKE_ID = "1%020d"


def get(o, *path):
    for p in path:
        if not isinstance(o, list) or p >= len(o):
            return None
        o = o[p]
    return o


def put(o, value, *path):
    parent = get(o, *path[:-1]) if len(path) > 1 else o
    if isinstance(parent, list) and path[-1] < len(parent) and isinstance(parent[path[-1]], str):
        parent[path[-1]] = value


def is_author_block(a):
    # [?, ?, [".../maps/contrib/<id>/reviews..."], ?, ?, [name, avatar, [profile], id, ...]]
    if not isinstance(a, list) or len(a) < 6:
        return False
    link = get(a, 2, 0)
    return isinstance(link, str) and "maps/contrib/" in link and isinstance(get(a, 5, 0), str)


def anonymize(raw):
    prefix = ""
    body = raw
    if raw.startswith(")]}'"):
        prefix, _, body = raw.partition("\n")
        prefix += "\n"
    data = json.loads(body)
    counter = [0]
    ids = {}

    def fake_id(real):
        if real not in ids:
            ids[real] = FAKE_ID % (len(ids) + 1)
        return ids[real]

    def walk(o):
        if not isinstance(o, list):
            return
        if is_author_block(get(o, 1, 4)):  # o is a review element
            counter[0] += 1
            n = counter[0]
            a = get(o, 1, 4)
            put(a, f"Reviewer {n}", 5, 0)
            put(a, f"https://lh3.googleusercontent.com/a/anonymous-{n}", 5, 1)
            put(o, f"Redacted review text {n}.", 2, 15, 0, 0)
            put(o, f"Redacted review text {n} (translation).", 2, 15, 1, 0)
            put(o, "Redacted owner reply.", 3, 14, 0, 0)
            put(o, "Redacted owner reply (translation).", 3, 14, 1, 0)
        for x in o:
            walk(x)

    walk(data)
    text = json.dumps(data, ensure_ascii=False, separators=(",", ":"))
    found = set(ID_RE.findall(text))
    d = get(data, 6)
    for slot in (2, 8):  # business owner account id
        v = get(d, 57, slot)
        if isinstance(v, str) and re.fullmatch(r"\d{15,25}", v):
            found.add(v)
    for real in sorted(found):
        text = text.replace(real, fake_id(real))
    return prefix + text


def main(paths):
    for p in paths:
        with open(p, encoding="utf-8") as f:
            raw = f.read()
        out = anonymize(raw)
        if out != raw:
            with open(p, "w", encoding="utf-8") as f:
                f.write(out)
        print(f"{p}: ok")


if __name__ == "__main__":
    main(sys.argv[1:])
