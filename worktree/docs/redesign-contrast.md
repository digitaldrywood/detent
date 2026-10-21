# Redesign contrast audit (WCAG AA)

Generated from the `@theme` tokens in `static/css/input.css`. AA requires
4.5:1 for normal text; the smallest type in the app is 11px, so every
pair is held to the normal-text bar. Values the design brief suggested
that fell short were adjusted with hues preserved: dark `dim`, and light
`sec`, `dim`, `ok`, `warn`, `err`, `accent`.

## Dark (default)

| Foreground | On page | On surface | On elev |
|---|---|---|---|
| `text` #EDF0F4 | 17.02 | 15.71 | 14.29 |
| `sec` #8A93A2 | 6.28 | 5.80 | 5.27 |
| `dim` #808A97 | 5.56 | 5.13 | 4.67 |
| `ok` #34D399 | 10.12 | 9.34 | 8.50 |
| `warn` #FBBF24 | 11.66 | 10.76 | 9.79 |
| `err` #F87171 | 7.03 | 6.49 | 5.91 |
| `info` #60A5FA | 7.65 | 7.07 | 6.43 |
| `accent` #2DD4BF | 10.45 | 9.65 | 8.78 |

| Status chip (text on 15% tint over elev) | Ratio |
|---|---|
| `ok` on `ok/15` | 6.26 |
| `warn` on `warn/15` | 7.01 |
| `err` on `err/15` | 4.73 |
| `info` on `info/15` | 4.96 |

Exception strip (`err/9` over page): text 15.44, sec 5.69.
Accent action button (page-colored text on accent): 10.45.

## Light (`[data-theme="light"]`)

| Foreground | On page | On surface | On elev |
|---|---|---|---|
| `text` #1A1F27 | 15.57 | 16.55 | 14.75 |
| `sec` #5D6B80 | 5.09 | 5.41 | 4.83 |
| `dim` #5F6B7E | 5.08 | 5.40 | 4.81 |
| `ok` #046C4E | 6.06 | 6.44 | 5.74 |
| `warn` #92400E | 6.67 | 7.09 | 6.32 |
| `err` #B42318 | 6.19 | 6.57 | 5.86 |
| `info` #1D4ED8 | 6.31 | 6.70 | 5.98 |
| `accent` #0F766E | 5.15 | 5.47 | 4.88 |

| Status chip (text on 15% tint over elev) | Ratio |
|---|---|
| `ok` on `ok/15` | 4.62 |
| `warn` on `warn/15` | 5.03 |
| `err` on `err/15` | 4.59 |
| `info` on `info/15` | 4.75 |

Exception strip (`err/9` over page): text 13.47, sec 4.40.
Accent action button (page-colored text on accent): 5.15.

All listed pairs meet or exceed 4.5:1.
