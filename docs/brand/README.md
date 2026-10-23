# Panko logo kit

The white infrastructure cube sits inside a segmented orange wrapper. Keep that relationship intact when using the logo.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="panko-horizontal-dark.svg">
  <img src="panko-horizontal-light.svg" alt="Panko horizontal logo" width="444">
</picture>

## Choose an asset

Every named SVG below has a matching PNG in this folder. SVG is the preferred format for READMEs and documentation; PNG works for tools that cannot display SVG.

| Use | Asset | Size / background |
| --- | --- | --- |
| README, documentation header | [Horizontal, light](panko-horizontal-light.svg) / [dark](panko-horizontal-dark.svg) | 740 × 280; transparent. Theme refers to the intended background. |
| Cover, centered project page | [Stacked, light](panko-stacked-light.svg) / [dark](panko-stacked-dark.svg) | 640 × 790; transparent. |
| Avatar, project icon | [Color mark](panko-mark-color.svg) | 640 × 640; transparent, with opaque white cube faces. |
| One-color mark | [Ink](panko-mark-black.svg) / [white](panko-mark-white.svg) | 640 × 640; transparent cutouts. “Black” uses the brand's dark ink. |
| One-color wordmark | [Ink](panko-horizontal-black.svg) / [white](panko-horizontal-white.svg) | 740 × 280; transparent. |
| Social / repository preview | [Social card PNG](panko-social.png) / [SVG](panko-social.svg) | 1280 × 640; opaque dark background. Upload the PNG in repository social-preview settings. |
| Browser favicon | [SVG](favicon.svg) / [ICO](favicon.ico) | Simplified mark; ICO contains 16, 32, and 48 px PNG images. |
| Small raster icons | [16](panko-icon-16.png), [32](panko-icon-32.png), [48](panko-icon-48.png) | Transparent, simplified mark without the six radial notches. |
| Apple touch / app icons | [180](panko-icon-180.png), [192](panko-icon-192.png), [512](panko-icon-512.png) | Transparent square PNGs; add an opaque background if a target platform requires one. |
| Extracted raster reference | [Transparent source extraction](panko-mark-extracted.png) | AI-assisted extraction from the supplied raster; vector exports above are the canonical docs assets. |

Original supplied artwork remains at [`/logo.png`](../../logo.png) and [`/logo-large.png`](../../logo-large.png). The SVGs are a clean geometric redraw, preserving the cube, segmented wrapper, orange palette, and gray outline; they are not a pixel-exact trace of the shaded raster originals.

## Embed in a README

Paths below assume a README at the repository root:

```html
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/brand/panko-horizontal-dark.svg">
  <img src="docs/brand/panko-horizontal-light.svg" alt="Panko" width="444">
</picture>
```

For a simple mark:

```markdown
![Panko](docs/brand/panko-mark-color.svg)
```

Use dark lettering on light backgrounds and white lettering on dark backgrounds. For one-color printing, use the monochrome files rather than converting the color artwork to grayscale. Preserve the aspect ratio and leave clear space of at least one-tenth of the mark's width around it. At very small sizes, use the simplified favicon artwork. Avoid placing the logo over busy photographs.

## Palette and typography

| Element | Color |
| --- | --- |
| Orange gradient | `#FFB000` → `#CD7100` |
| Cube outline | `#999999` |
| Cube faces / reversed lettering | `#FFFFFF` |
| Lettering / ink variant | `#17212B` |
| Social card background | `#111B26` |

SVG wordmarks use bold `DejaVu Sans` with a generic sans-serif fallback. They contain editable text, so the exact lettering may vary between systems. Use the accompanying PNG for fixed typography. The social card uses the same font family.

## Rebuild

From the repository root, with Python 3 and `rsvg-convert` installed:

```sh
python3 tools/build_brand.py
```

The script writes SVGs, renders the matching PNGs and six icon sizes, and packages the small PNGs into `favicon.ico`. It does not change the original raster files or the extracted reference. The artwork geometry and layout live in the script; edit them there before rebuilding.

The transparent raster reference was produced with the built-in image generation tool using the [recorded extraction prompt](extraction-prompt.txt). The vector family and its raster exports are reproducible from code without image generation. Asset usage rights remain subject to the project's eventual license and any separately specified brand terms; this kit does not introduce a license.
