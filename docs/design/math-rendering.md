# Math rendering — LaTeX in the terminal (inline Unicode + 2D display blocks)

## Goal

Render the LaTeX math that models emit (`$...$`, `$$...$$`, `\(...\)`, `\[...\]`)
in the terminal markdown stream:

- **Inline math** (`$...$`, `\(...\)`) inside a text line → a single-line
  **Unicode approximation** (greek, operators, super/subscripts). A text line
  cannot grow taller, so no 2D here.
- **Display math** (`$$...$$`, `\[...\]`) as its own block → a **2D
  monospace layout** (stacked fractions, roots with a drawn vinculum, matrices,
  sums/integrals with limits), à la SymPy's pretty-printer.

Graphics protocols (sixel/kitty/iTerm2) are **out**: research confirmed
Terminal.app supports none of them, and every LaTeX→image engine drags in a
TeX/Node/Python runtime. Pure-text 2D layout works on every terminal with zero
runtime deps.

## Library placement

A new **self-maintained `internal/` package** (like the vendored UI stack), not
a third-party dependency — no permissively-licensed Go library does true 2D math
layout. Two BSD-3 sources are ported/trimmed with attribution (both
license-compatible with MIT; keep NOTICE):

- **go-latex/latex** (BSD-3, archived) — its `scanner`→`parser`→`ast` pipeline
  and especially its `internal/tex2unicode` symbol table (macro-name → rune) are
  lifted as the seed.
- **SymPy `stringpict.py`/`pretty.py`** (BSD-3) — the box-model *algorithm* is
  ported (no code, different language); it is the reference for the layout
  engine.

GPL sources (libtexprintf, asciiTeX) are **reference only**, never vendored.

Proposed layout:

```
internal/mathtext/
  symbols.go   — macro/greek/operator → Unicode tables (seed: tex2unicode)
  delim.go     — normalize $ $$ \( \) \[ \] to canonical inline/display
  parse.go     — recursive-descent parser over the Tier-1 math subset → AST
  box.go       — the Box{lines,baseline,width} model + 3 primitives
  layout.go    — per-node 2D renderers (frac, sqrt, sup/sub, delim, sum…)
  inline.go    — single-line Unicode approximation (no 2D)
  render.go    — public API
```

Public API:

```go
// ApproxInline renders inline math to a single Unicode line (never multi-line);
// unsupported bits degrade to cleaned linear source.
func ApproxInline(latex string) string

// Render2D lays out display math into a multi-line block that fits within
// width columns. Returns the cleaned source (ok=false) when the formula can't
// be parsed or laid out, so the caller can fall back gracefully.
func Render2D(latex string, width int) (block string, ok bool)
```

## The box model (ported SymPy algorithm)

Every subexpression is a `Box{ lines []string; baseline int; width int }` — a
fixed-width char grid plus one **baseline** row (its vertical reference). Widths
are measured with **uniseg** (the same ruler tables/quotes use), so `\text{中文}`
and emoji in math align. Three primitives compose everything:

1. **hconcat** — place boxes side by side aligned on a shared baseline (pad above
   to the max baseline, below to the max depth).
2. **vstack** — center boxes to a common width, stack top-to-bottom, recompute
   baseline (used for fractions: numerator / bar / denominator, baseline on the
   bar).
3. **raise/lower** — shift a small box up (superscript) or down (subscript)
   relative to the base's baseline, then hconcat.

Everything else is glyph mapping. ~11 node types cover ~90% of LLM output:
atoms, binary/relational ops, `\frac`, `^`, `_`, `\sqrt`/`\root`, auto-scaled
`()[]` delimiters, `\sum`/`\prod`/`\int` with limits, `pmatrix`/`bmatrix`,
`cases`, `\lim`.

**Drawn shapes, never combining marks** (a load-bearing decision that echoes the
emoji/CJK-width work): the fraction bar (`─`), overline, tall delimiters
(`⎛⎜⎝`/`⎡⎢⎣`/`⎧⎨⎩`), tall `∫`/`∑`/`∏`, and the **scalable radical** are **drawn
across rows** from corner+extension pieces — U+0305-style combining marks render
unreliably in terminals and wreck monospace width. The display radical is a
rising `╱` stroke (one per radicand row) climbing from a `╲` base to an
underscore (`_`) vinculum, so it stays connected however tall the radicand — the
single-glyph `√` (which cannot grow, leaving a detached "half radical" over a
multi-row body) is used only in the single-line inline approximation. Greek and
digit super/subscripts are likewise single-glyph forms, used where a real code
point exists.

## Inline approximation (single line)

`ApproxInline`: greek macros → α/β/…, operators → ×÷±≤≥≈≠∞, super/subscripts →
Unicode super/subscript glyphs **where they exist** (digits, `+−=()`, most
lowercase superscripts; subscripts cover far fewer letters). When the glyph does
**not** exist (capitals, `x_j`, `C_B`) it falls back to **linear** `x_j` — no
combining marks. `\frac{a}{b}` → `a/b`. Unparseable inline → cleaned source.

## Integration into chat/markdown.go

**Display block** (mirrors the fenced-code block):

- New state on `markdownWriter`: `inMath bool`, `mathLines []string`,
  `mathView *promptui.StreamView`.
- In `Write()`, detect a line that is `$$` (or `\[`) to open, and `$$` (or `\]`)
  to close — placed **after the quote check, before the table check**, and it
  flushes/interrupts a pending table/list/quote like a code fence does.
- `flushMath()` mirrors `flushCode()`: clear the preview, `beginBlock()`, write
  `Render2D(joined, m.termWidth())` (or the cleaned-source fallback), `endBlock()`
  — so a display formula is a `unitBlock` with one blank line above and below,
  and it rides the recursive quote child writer + width override automatically
  (math inside a blockquote fits inside the bar, at the reduced inner width).
- Too wide to fit: best-effort (render at width, let it overflow) with the
  fallback to linear source documented; a later phase can add shrink/scroll.

**Inline span** (in `highlightInline`, between the backtick and bold branches):

- Detect `$...$` (and `\(...\)`), guarding: `\$` literal dollar, currency like
  `$5` (a `$` followed by a digit / not closed on the same run is not math),
  and code spans win first (the backtick branch runs earlier). Replace with
  `ApproxInline(...)`.
- Works inside table cells (styledCell calls highlightInline) — single-line, so
  it never breaks row height.

**NoColor**: the layout is glyph-based and color-agnostic; optional dim styling
only, gated by the existing renderer profile.

## Phases (each independently shippable)

- **P1 — foundation + inline.** The `internal/mathtext` package skeleton, symbol
  tables (seed tex2unicode), delimiter normalization, `ApproxInline`, and wiring
  into `highlightInline` with all the escaping/currency/code-span edge cases.
  Ships: inline math reads well; complex inline degrades to clean source. Small,
  de-risks the delimiter/edge-case surface before the big engine.
- **P2 — parser + 2D engine + display blocks.** *(Shipped.)* The recursive-descent
  parser over the Tier-1 subset (`parse.go`/`macros.go`), the stringPict `Box`
  model + primitives (`box.go`), the per-node 2D renderers (`layout.go`), and
  `Render2D` (`render.go`). Display math (`$$…$$` and `\[…\]`, both the multi-line
  fence form and the one-line form) is buffered in the `markdownWriter.Write`
  loop with a live "rendering math…" preview and a beginBlock/endBlock boundary
  (a block unit with one blank line above and below), riding the blockquote child
  writer's width override; an unparseable formula falls back to the cleaned linear
  source (a dim line), and a too-wide formula still renders (best-effort overflow,
  shrink deferred to P3). Every drawn shape (fraction bar, vinculum, tall
  delimiters, tall integrals) is a box-drawing glyph — never a combining mark.
  Ships: the headline stacked fractions / roots with a drawn vinculum / matrices /
  sums-and-integrals with limits.
- **P3 — coverage + polish.** *(Shipped.)* Broadened the display-math subset so
  real-world formulas render 2D instead of falling back, and made the remaining
  fallback readable:
  - **Drawn accents** — `\hat`/`\bar`/`\vec`/`\tilde`/`\dot`/`\ddot` render as a
    glyph ROW above the base (an `Accent` node, drawn like the sqrt vinculum),
    never a combining mark (U+0300–U+036F is absolute-forbidden).
  - **Named functions & delimiters** — `\exp`/`\log`/`\sin`… as operator names,
    `\left(…\right)` auto-sizing, literal `\{`/`\}`, `\mid`/`\|` relations,
    manual spacing (`\,` `\!` `\:` `\;` `\quad` `\qquad`) dropped.
  - **Math fonts** — `\mathbf`/`\mathrm` degrade to plain letters (the box layout
    carries no SGR); `\mathbb`/`\mathcal`/`\mathfrak` map to their Unicode
    math-alphanumeric variant (`\mathcal{P}` → `𝒫`, `\mathbb{R}` → `ℝ`), with the
    Letterlike-Symbols holes handled.
  - **Aligned environments** — `aligned`/`align`/`split`/`gathered`/`eqnarray`
    reuse the `Matrix` node (rows lined up on `&`, no brackets); `\\[4pt]`/`\\*`
    row-break options are dropped.
  - **Improved fallback** — `CleanSource` now delegates to the inline
    approximator (`ApproxInline`) instead of dumping raw TeX. An unrenderable
    formula surfaces as readable math-ish text: `\frac{a}{b}` → `a/b`, `\sqrt{x}`
    → `√(x)`, greek/operators → their glyph, scripts → super/subscript runes,
    `\text{}`/`\mathbf{}`/`\mathcal{}` unwrapped, layout/size/spacing macros
    stripped, `\overbrace`/`\underbrace`/`\phantom` unwrapped/dropped, and any
    unknown macro keeps its name without the backslash — never `\sqrt{b^2-4ac}`.
  - **Cases** — `\begin{cases}…\end{cases}` lays out 2D: rows aligned on `&`
    (condition column after the value), wrapped in an auto-sized left brace
    (`⎧⎨⎩`), reusing the `Matrix`/`Delim` machinery. `\binom{n}{k}` is still a
    linear fallback (`C(n, k)`), not a 2D vertical binomial.
  - **Still out of scope** (→ clean fallback, not raw): `\overbrace`/`\underbrace`
    2D braces, `\binom` 2D vertical form, `\phantom` spacing, tensor multi-index,
    commutative diagrams, continued fractions. Display-block shrink for very wide
    formulas and real KaTeX in the HTML export path remain deferred.

## Non-goals

- No graphics-protocol image rendering (Terminal.app can't; heavy deps).
- No combining-mark stacking (unreliable + width-breaking).
- Long tail punted to the cleaned-source fallback: commutative diagrams,
  `\overbrace`/`\underbrace` 2D braces, tensor multi-index, continued fractions,
  `\phantom`/arbitrary spacing. (The `aligned`/`align`/`split`/`gathered`
  environments now render as of P3 — they are no longer punted.)

## Testing

Pure-function core (no terminal): symbol mapping tables; delimiter
normalization (all four forms + GPT's inconsistent backslashes); `ApproxInline`
(greek/ops/super/sub with linear fallback, `\$`/currency/code-span edges); the
box primitives (hconcat/vstack/raise baseline math); each node renderer against
golden 2D layouts (frac, nested frac, sqrt with vinculum, sum with limits, 2×2
matrix); the unparseable/too-wide fallback returns clean source; width measured
with uniseg (CJK in `\text{}`); NoColor unaffected. Integration: a `$$` block is
a block unit (blank line above/below), rides the quote child writer, streams
byte-identically in chunks.
