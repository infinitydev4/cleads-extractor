Parser regression fixtures.

- `place_raw.json`, `place_raw2.json`: copied verbatim from
  gosom/google-maps-scraper `testdata/` (MIT License, Copyright (c) 2023
  Georgios Komninos). They are already public upstream and include reviews:
  do not add more such files without anonymizing them.
- `search_live*.json`, `preview_place_live.json`: captured from live Google
  Maps responses (business listings only). Captures that contain reviews go
  through `scripts/anonymize_fixture.py` first, which removes author names,
  Google account ids, avatars and review texts.
