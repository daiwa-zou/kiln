#!/usr/bin/env bash
#
# Render a coverage badge as a static SVG committed to the repo.
#
# kiln is a private repo, so shields.io cannot read its state and Codecov
# badges would render as broken images -- GitHub proxies external images
# anonymously. A committed SVG referenced by a relative path resolves through
# the viewer's own session instead, so it works for anyone with repo access.
#
#   usage: scripts/coverage-badge.sh [profile] [output]
#
set -euo pipefail

profile="${1:-coverage.out}"
output="${2:-.github/badges/coverage.svg}"

if [[ ! -f $profile ]]; then
	echo "coverage-badge: no profile at $profile (run: make cover)" >&2
	exit 1
fi

# `go tool cover -func` ends with a "total:" line: total: (statements) 78.4%
pct=$(go tool cover -func="$profile" | awk '/^total:/ { gsub(/%/, "", $NF); print $NF }')
if [[ -z $pct ]]; then
	echo "coverage-badge: could not parse a total from $profile" >&2
	exit 1
fi

color=$(awk -v p="$pct" 'BEGIN {
	if      (p <  50) print "#e05d44"
	else if (p <  70) print "#fe7d37"
	else if (p <  80) print "#dfb317"
	else if (p <  90) print "#a4a61d"
	else              print "#4c1"
}')

label="coverage"
value="${pct}%"

# Verdana 11px averages ~7px/char; +10px padding per side matches shields' flat
# style closely enough that the badge does not look homemade.
label_w=$((${#label} * 7 + 10))
value_w=$((${#value} * 7 + 10))
total_w=$((label_w + value_w))

# Text is drawn at 10x and scaled by .1 so glyph positioning stays sub-pixel.
label_x=$((label_w * 5))
value_x=$((label_w * 10 + value_w * 5))
label_len=$(((label_w - 10) * 10))
value_len=$(((value_w - 10) * 10))

mkdir -p "$(dirname "$output")"
cat >"$output" <<SVG
<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" width="${total_w}" height="20" role="img" aria-label="${label}: ${value}">
  <title>${label}: ${value}</title>
  <linearGradient id="s" x2="0" y2="100%">
    <stop offset="0" stop-color="#bbb" stop-opacity=".1"/>
    <stop offset="1" stop-opacity=".1"/>
  </linearGradient>
  <clipPath id="r"><rect width="${total_w}" height="20" rx="3" fill="#fff"/></clipPath>
  <g clip-path="url(#r)">
    <rect width="${label_w}" height="20" fill="#555"/>
    <rect x="${label_w}" width="${value_w}" height="20" fill="${color}"/>
    <rect width="${total_w}" height="20" fill="url(#s)"/>
  </g>
  <g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" text-rendering="geometricPrecision" font-size="110">
    <text x="${label_x}" y="150" fill="#010101" fill-opacity=".3" transform="scale(.1)" textLength="${label_len}">${label}</text>
    <text x="${label_x}" y="140" transform="scale(.1)" textLength="${label_len}">${label}</text>
    <text x="${value_x}" y="150" fill="#010101" fill-opacity=".3" transform="scale(.1)" textLength="${value_len}">${value}</text>
    <text x="${value_x}" y="140" transform="scale(.1)" textLength="${value_len}">${value}</text>
  </g>
</svg>
SVG

echo "coverage-badge: ${pct}% -> ${output}"
