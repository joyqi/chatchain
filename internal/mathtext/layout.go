package mathtext

import (
	"strings"
)

// This file lays out an AST Node (parse.go) into a Box (box.go): the 2D
// monospace geometry of a display formula. Every node type has a renderer that
// composes the box primitives (HConcat / VStack / Raise / Lower / Overline /
// HRule / LeftDelim / RightDelim). The result is a single Box whose Lines are
// the final rows the terminal prints.
//
// The layout is a hand-rolled port of SymPy's stringPict/prettyForm pretty
// printer (sympy/printing/pretty/*, BSD-3-Clause): the geometry — fractions
// centered over a drawn bar with the baseline on the bar, radicals with a drawn
// vinculum, big operators with limits stacked above and below on a shared
// column, matrices aligned per-column and wrapped in auto-sized delimiters — is
// the reference ALGORITHM. No SymPy code is copied; it was reimplemented in Go
// against this package's own Box model, and every width is measured with
// rivo/uniseg (the single ruler the whole renderer shares) so CJK inside
// \text{} and wide glyphs align.
//
// No combining marks (U+0300–U+036F) are ever emitted: bars, vinculums, tall
// delimiters, and tall operators are DRAWN across rows from box-drawing glyphs
// (see box.go), which is why the layout is glyph-based and color-agnostic.

// layout renders a Node to its Box. A nil node is the empty box (an absent
// script or an empty group contributes nothing).
func layout(n Node) Box {
	switch v := n.(type) {
	case nil:
		return EmptyBox()
	case *Seq:
		return layoutSeq(v)
	case *Atom:
		return layoutAtom(v)
	case *Text:
		return layoutText(v)
	case *Frac:
		return layoutFrac(v)
	case *Sqrt:
		return layoutSqrt(v)
	case *Sup:
		return layoutSup(v)
	case *Sub:
		return layoutSub(v)
	case *SupSub:
		return layoutSupSub(v)
	case *Delim:
		return layoutDelim(v)
	case *BigOp:
		return layoutBigOp(v, 1)
	case *Matrix:
		return layoutMatrix(v)
	case *Accent:
		return layoutAccent(v)
	default:
		// An unrecognized node should not reach here (the parser only emits the
		// set above); render nothing rather than panic.
		return EmptyBox()
	}
}

// layoutAtom renders a resolved atom as a single-row box. Multi-glyph atoms
// (a digit run "128", a named function "sin", a mapped symbol) stay on one row;
// the box width is the uniseg display width so CJK/wide runes are accounted for.
func layoutAtom(a *Atom) Box {
	if a.Text == "" {
		return EmptyBox()
	}
	return NewBox(a.Text)
}

// layoutText renders \text{...} prose verbatim as one row, uniseg-measured so
// CJK inside the text aligns with the surrounding grid.
func layoutText(t *Text) Box {
	if t.S == "" {
		return EmptyBox()
	}
	return NewBox(t.S)
}

// layoutAccent draws an accent as a glyph ROW ABOVE the base box (never a
// combining mark): \hat -> '^', \vec -> '→', \tilde -> '~', \dot -> '·',
// \ddot -> '··', all centered over the base; \bar/\overline reuse the full-width
// drawn rule (Overline). The base keeps its own baseline, shifted down by the
// inserted accent row, so the accented atom still sits on the surrounding
// baseline. This mirrors the sqrt vinculum: a drawn row, glyph-plain.
func layoutAccent(a *Accent) Box {
	base := layout(a.Base)
	if base.Height() == 0 {
		base = NewBox(" ")
	}
	glyph, fullWidth := accentGlyph(a.Kind)
	if fullWidth {
		return Overline(base) // \bar / \overline span the whole width
	}
	return AccentRow(base, glyph)
}

// binaryOps is the set of single-rune binary/relational operators that get a
// thin space on each side when they appear between operands in a Seq, matching
// SymPy's spacing: "a + b", "x = y", "n ≤ m". Ordinary juxtaposition (a letter
// beside a letter, an implicit product "2x") gets no space. The set covers the
// ASCII operators plus the Unicode glyphs the symbol tables map to.
var binaryOps = map[rune]bool{
	'+': true, '=': true, '<': true, '>': true,
	'×': true, '÷': true, '±': true, '∓': true, '⋅': true, '∗': true,
	'≤': true, '≥': true, '≠': true, '≈': true, '≡': true, '∼': true,
	'≃': true, '≅': true, '∝': true, '≪': true, '≫': true,
	'∈': true, '∉': true, '∋': true, '⊂': true, '⊆': true, '⊃': true,
	'⊇': true, '∪': true, '∩': true, '∧': true, '∨': true,
	'∣': true, '∤': true, '‖': true, '∥': true, '≍': true, '≐': true,
	'→': true, '←': true, '↔': true, '⇒': true, '⇐': true, '⇔': true, '↦': true,
	'⊕': true, '⊗': true, '⊙': true, '∘': true, '∖': true,
}

// isBinaryOpBox reports whether box b is a lone binary/relational operator that
// wants surrounding thin space. It is a single-row, single-operator box; the
// leading '-' (subtraction) is included even though a leading '-' can be unary,
// because in a run between two operands it reads as subtraction.
func isBinaryOpBox(b Box) bool {
	if b.Height() != 1 {
		return false
	}
	rs := []rune(b.Lines[b.Baseline])
	if len(rs) != 1 {
		return false
	}
	if rs[0] == '-' {
		return true
	}
	return binaryOps[rs[0]]
}

// layoutSeq lays out a run of nodes left to right, inserting a single space
// around a binary/relational operator that sits between two operands. A leading
// or trailing operator (a unary sign, a relation with a missing side) gets no
// space on the missing side. This mirrors SymPy: juxtaposition is tight, binary
// operators breathe.
//
// A glyph big operator (∑/∏/∫) grows to the height of the operand that follows
// it: the parser leaves that operand as the next sibling, so this loop lays the
// sibling out first and passes its height into layoutBigOp, then places a tight
// pair of operator+operand (a word operator like \lim gets a thin space before
// its operand).
func layoutSeq(s *Seq) Box {
	if len(s.Items) == 0 {
		return EmptyBox()
	}
	// Pre-render every item into a box; a BigOp folds its following sibling in so
	// a glyph operator can grow to the operand's height.
	var boxes []Box
	for i := 0; i < len(s.Items); i++ {
		it := s.Items[i]
		if op, ok := it.(*BigOp); ok {
			// Lay out the following operand (if any) to size the operator glyph.
			operand := EmptyBox()
			if i+1 < len(s.Items) {
				operand = layout(s.Items[i+1])
			}
			h := 1
			if operand.Height() > h {
				h = operand.Height()
			}
			opBox := layoutBigOp(op, h)
			if operand.Height() > 0 {
				if op.Word != "" {
					opBox = HConcat(opBox, spaceBox(), operand)
				} else {
					opBox = HConcat(opBox, operand)
				}
				i++ // the operand was folded into the operator box
			}
			boxes = append(boxes, opBox)
			continue
		}
		b := layout(it)
		if b.Height() == 0 || b.Width == 0 {
			continue
		}
		boxes = append(boxes, b)
	}
	if len(boxes) == 0 {
		return EmptyBox()
	}
	var parts []Box
	for i, b := range boxes {
		if isBinaryOpBox(b) {
			// A thin space hugs the operator on whichever side has an operand.
			// Guarding both sides independently (rather than requiring the op to
			// be strictly interior) keeps the space after a relation that opens
			// an alignment cell — "&= y" must read "= y", not "=y".
			if i > 0 {
				parts = append(parts, spaceBox())
			}
			parts = append(parts, b)
			if i < len(boxes)-1 {
				parts = append(parts, spaceBox())
			}
			continue
		}
		parts = append(parts, b)
	}
	return HConcat(parts...)
}

// spaceBox is a one-column blank box on baseline 0, the thin space inserted
// around binary operators.
func spaceBox() Box { return NewBox(" ") }

// layoutFrac lays out \frac{Num}{Den}: the numerator centered over a drawn bar
// over the centered denominator, with the baseline on the bar. The bar spans
// the wider of the two parts (SymPy's rule), so a wide denominator widens the
// bar and centers the narrow numerator over it.
func layoutFrac(f *Frac) Box {
	num := layout(f.Num)
	den := layout(f.Den)
	w := num.Width
	if den.Width > w {
		w = den.Width
	}
	if w < 1 {
		w = 1
	}
	bar := HRule(w)
	// The baseline lands on the bar row: numerator rows come first, then the
	// single bar row, so baselineRow == num.Height().
	return VStack([]Box{num, bar, den}, num.Height())
}

// layoutSqrt lays out a radical: a drawn vinculum (Overline) over the radicand
// with the √ radical sign attached at the lower left, its top corner meeting
// the vinculum. The radicand's baseline is preserved so the whole radical sits
// on the surrounding baseline. The optional degree index (\sqrt[n]{}) is placed
// small at the radical's upper left.
//
// Shape for a two-row radicand (the check GROWS with the radicand height, so the
// stroke always reaches the vinculum — no disconnected "half radical"):
//
//	   _____
//	  ╱  2
//	╲╱  x  + 1
//
// The radical is drawn from box-drawing diagonals (no combining marks): a rising
// ╱ stroke, one per radicand row, climbing left-to-right from the ╲ at the base
// to meet the vinculum. The vinculum is an underscore run (_), which paints on
// the cell's bottom edge — so it sits directly on top of the radicand AND lines
// up with the top ╱ tip, closing the gap the old √+─ pair left open.
func layoutSqrt(s *Sqrt) Box {
	rad := layout(s.Radicand)
	if rad.Height() == 0 {
		rad = NewBox(" ")
	}
	body := sqrtBody(rad)
	if s.Index == nil {
		return body
	}
	// Degree index: place it small at the upper left of the radical (in the
	// crook above the ╲), rising above the baseline like a superscript.
	idx := layout(s.Index)
	if idx.Height() == 0 {
		return body
	}
	return HConcat(scriptColumn(body, idx, EmptyBox(), 0), body)
}

// sqrtBody wraps a laid-out radicand in a scalable radical sign. The left wedge
// is h+1 columns wide (h = radicand height): a ╲ at the bottom-left corner and a
// ╱ per row climbing to the top, so the stroke meets the vinculum whatever the
// radicand's height. The vinculum is a row of underscores over the radicand.
func sqrtBody(rad Box) Box {
	h := rad.Height()
	w := rad.Width
	wedge := h + 1 // ╲ column + one ╱ per row

	lines := make([]string, h+1)
	// Vinculum row: blanks under the wedge, then '_' across the radicand (plus a
	// leading gap column so the underscore starts above the topmost ╱ tip).
	lines[0] = strings.Repeat(" ", wedge) + strings.Repeat("_", w+1)
	for r := 0; r < h; r++ {
		arm := []rune(strings.Repeat(" ", wedge))
		arm[wedge-1-r] = '╱' // ╱ climbs right→left going up (row 0 is highest)
		if r == h-1 {
			arm[0] = '╲' // the base of the check
		}
		lines[r+1] = string(arm) + " " + rad.Lines[r]
	}
	return Box{Lines: lines, Baseline: rad.Baseline + 1, Width: wedge + 1 + w}
}

// layoutSup lays out Base^Exp with the exponent to the upper right of the base.
// The exponent occupies its OWN rows strictly above the base's baseline (the
// box primitives only shift a baseline, they never grow a box, so a raised
// script is built as an explicit multi-row column that HConcat then aligns).
func layoutSup(s *Sup) Box {
	base := layout(s.Base)
	exp := layout(s.Exp)
	if exp.Height() == 0 {
		return base
	}
	return HConcat(base, scriptColumn(base, exp, EmptyBox(), accentLift(s.Base)))
}

// layoutSub lays out Base_Sub with the subscript to the lower right of the base.
func layoutSub(s *Sub) Box {
	base := layout(s.Base)
	sub := layout(s.Sub)
	if sub.Height() == 0 {
		return base
	}
	return HConcat(base, scriptColumn(base, EmptyBox(), sub, 0))
}

// layoutSupSub stacks a superscript and a subscript on the SAME base column: the
// sup rows above the base's baseline row, the sub rows below it, sharing one
// script column to the right of the base.
func layoutSupSub(s *SupSub) Box {
	base := layout(s.Base)
	sup := layout(s.Sup)
	sub := layout(s.Sub)
	if sup.Height() == 0 && sub.Height() == 0 {
		return base
	}
	return HConcat(base, scriptColumn(base, sup, sub, accentLift(s.Base)))
}

// accentLift reports the extra rows a superscript must rise to clear an accent
// drawn over the base. An accent (\hat, \dot, …) adds a thin glyph row directly
// above the base; a superscript at the default one-row lift lands beside that
// same row and the two read as one token (x̂² collapsing to "^2"). Lifting the
// script by the accent's ascent floats it cleanly above the accent. Non-accent
// bases return 0 — ordinary and tall bases keep their conventional placement,
// and a subscript never needs the lift (the accent sits on top, not below).
func accentLift(base Node) int {
	if _, ok := base.(*Accent); ok {
		return layout(base).Baseline
	}
	return 0
}

// scriptColumn builds the box that HConcat places to the right of base to carry
// a superscript (sup) above the base baseline and a subscript (sub) below it.
// Either script may be empty. The column has:
//
//   - the sup's rows on top,
//   - one anchor row aligned with the base's baseline (blank, so the base rune
//     shows through beside it),
//   - the sub's rows below,
//
// and its Baseline is the anchor row, so HConcat lines that anchor up with the
// base's baseline — lifting the sup a full row above and dropping the sub a full
// row below, on a clean monospace grid with no combining marks.
//
// supLift inserts that many extra blank rows between the sup and the anchor,
// raising the superscript further above the baseline (used to float a script
// clear of an accent drawn over the base — see accentLift).
func scriptColumn(base, sup, sub Box, supLift int) Box {
	w := sup.Width
	if sub.Width > w {
		w = sub.Width
	}
	if w < 1 {
		w = 1
	}
	rows := make([]string, 0, sup.Height()+supLift+1+sub.Height())
	for _, l := range sup.Lines {
		rows = append(rows, center(l, w))
	}
	for i := 0; i < supLift; i++ {
		rows = append(rows, blankLine(w))
	}
	anchor := len(rows)
	rows = append(rows, blankLine(w)) // the base-baseline row, blank in the script
	for _, l := range sub.Lines {
		rows = append(rows, center(l, w))
	}
	return Box{Lines: rows, Baseline: anchor, Width: w}
}

// delimKindFor maps a normalized delimiter string to its DelimKind and reports
// whether the side is drawn at all (a "." side is invisible → not drawn).
func delimKindFor(s string) (DelimKind, bool) {
	switch s {
	case "(", ")":
		return DelimParen, true
	case "[", "]":
		return DelimBracket, true
	case "{", "}":
		return DelimBrace, true
	case "|":
		return DelimBar, true
	case ".", "":
		return DelimParen, false // invisible side
	default:
		// Angle/floor/ceil (⟨⟩ ⌊⌋ ⌈⌉) have no tall box-drawing form; not one of
		// the standard kinds. layoutDelim draws them as the literal glyph so
		// they keep their meaning (a floor bracket must not read as a norm bar).
		return DelimParen, false
	}
}

// literalDelimBox draws a delimiter that has no extensible tall form as its
// literal glyph, sitting on the baseline row and padded to the inner height —
// correct for the common height-1 case and honest (right symbol) when taller.
func literalDelimBox(glyph string, height, baseline int) Box {
	if glyph == "" {
		return EmptyBox()
	}
	if height <= 1 {
		return NewBox(glyph)
	}
	lines := make([]string, height)
	w := boxWidth(glyph)
	for i := range lines {
		if i == baseline {
			lines[i] = glyph
		} else {
			lines[i] = strings.Repeat(" ", w)
		}
	}
	return Box{Lines: lines, Baseline: baseline, Width: w}
}

// layoutDelim lays out \left<L> Inner \right<R>: the inner box flanked by
// auto-sized delimiters whose height matches the inner box and whose baseline
// aligns with the inner box's baseline. A "." side is omitted.
func layoutDelim(d *Delim) Box {
	inner := layout(d.Inner)
	h := inner.Height()
	if h < 1 {
		h = 1
	}
	parts := make([]Box, 0, 3)
	if left := delimSide(d.Left, h, inner.Baseline, true); !isEmptyBox(left) {
		parts = append(parts, left)
	}
	parts = append(parts, inner)
	if right := delimSide(d.Right, h, inner.Baseline, false); !isEmptyBox(right) {
		parts = append(parts, right)
	}
	return HConcat(parts...)
}

// delimSide builds one side of a \left…\right pair: a standard kind uses the
// drawn tall delimiter; a bracket glyph without a tall form (⟨ ⌊ ⌈ …) uses its
// literal glyph; "." / unknown is omitted.
func delimSide(sym string, height, baseline int, left bool) Box {
	if sym == "" || sym == "." {
		return EmptyBox()
	}
	if kind, ok := delimKindFor(sym); ok {
		if left {
			return LeftDelim(kind, height, baseline)
		}
		return RightDelim(kind, height, baseline)
	}
	return literalDelimBox(sym, height, baseline)
}

// bigOpGlyph returns the single-glyph form of a big operator and its tall,
// multi-row drawn form. The tall form is used when the operand body is more
// than one row (a fraction, a matrix), matching SymPy's growth rule.
//
// Integral is drawn from the standard half-integral pieces (⌠ top, ⌡ bottom,
// ⎮ extension). Sum and product have no multi-row box-drawing form, so their
// tall version repeats the single glyph down the column — still an honest,
// combining-mark-free column.
// bigOpGlyph returns the drawn form of a big operator: its own single glyph,
// and a tall-column builder. The FAMILY (kind) picks the tall shape — integrals
// stack ⌠⎮⌡, every other N-ary operator repeats its own glyph down the column —
// while glyph is the operator-specific character so a union draws ⋃, not ∑.
func bigOpGlyph(kind, glyph string) (single string, tall func(height int) []string) {
	if kind == "int" {
		return glyph, func(height int) []string {
			if height < 2 {
				return []string{glyph}
			}
			rows := make([]string, height)
			rows[0] = "⌠"
			rows[height-1] = "⌡"
			for i := 1; i < height-1; i++ {
				rows[i] = "⎮"
			}
			return rows
		}
	}
	return glyph, func(height int) []string { return repeatGlyphColumn(glyph, height) }
}

// repeatGlyphColumn returns height rows each holding glyph (a plain vertical
// run used for the tall sum/product forms, which lack box-drawing pieces).
func repeatGlyphColumn(glyph string, height int) []string {
	if height < 1 {
		height = 1
	}
	rows := make([]string, height)
	for i := range rows {
		rows[i] = glyph
	}
	return rows
}

// layoutBigOp lays out a large operator with its limits. The operator glyph (a
// drawn ∑/∏/∫ column, or the upright word for \lim etc.) carries the upper limit
// centered ABOVE it and the lower limit centered BELOW it. The operand does NOT
// live inside the BigOp — the parser leaves it as the next sibling in the Seq,
// so layoutSeq places it to the right; bodyHeight is that sibling's row count
// (or 1 when the operator stands alone), used only to grow a glyph operator tall
// enough to bracket a multi-row integrand, matching SymPy's growth rule.
func layoutBigOp(op *BigOp, bodyHeight int) Box {
	var opBox Box
	if op.Word != "" {
		opBox = NewBox(op.Word) // \lim, \max, \det, …
	} else {
		// Only integrals grow to the operand's height (⌠⎮⌡ is a genuine
		// extensible sign). ∑ / ∏ / ⋃ / ⨁ … stay a SINGLE glyph regardless of
		// operand height — repeating the glyph down the column reads as several
		// separate operators, not one enlarged sign (SymPy keeps one glyph too).
		single, tall := bigOpGlyph(op.Op, op.Glyph)
		if op.Op == "int" && bodyHeight >= 2 {
			opBox = NewBoxLines(strings.Join(tall(bodyHeight), "\n"), bodyHeight/2)
		} else {
			opBox = NewBox(single)
		}
	}

	lower := layout(op.Lower)
	upper := layout(op.Upper)
	// Stack: upper limit, operator, lower limit; baseline stays on the operator.
	stackBoxes := make([]Box, 0, 3)
	baselineRow := 0
	if upper.Height() > 0 {
		stackBoxes = append(stackBoxes, upper)
		baselineRow = upper.Height()
	}
	stackBoxes = append(stackBoxes, opBox)
	// The operator's own baseline within the stack: its top row is baselineRow,
	// so the operator baseline row is baselineRow + opBox.Baseline.
	opBaseline := baselineRow + opBox.Baseline
	if lower.Height() > 0 {
		stackBoxes = append(stackBoxes, lower)
	}
	if len(stackBoxes) == 1 {
		return opBox
	}
	return VStack(stackBoxes, opBaseline)
}

// matrixDelims maps a matrix environment to the delimiter kinds that wrap it.
// The bool reports whether the side is drawn ("matrix" and "smallmatrix" have
// none; "cases" has only a left brace).
type matrixDelims struct {
	left, right DelimKind
	drawLeft    bool
	drawRight   bool
}

func matrixDelimsFor(env string) matrixDelims {
	switch env {
	case "pmatrix":
		return matrixDelims{DelimParen, DelimParen, true, true}
	case "bmatrix":
		return matrixDelims{DelimBracket, DelimBracket, true, true}
	case "Bmatrix":
		return matrixDelims{DelimBrace, DelimBrace, true, true}
	case "vmatrix":
		return matrixDelims{DelimBar, DelimBar, true, true}
	case "Vmatrix":
		return matrixDelims{DelimBar, DelimBar, true, true}
	case "cases":
		return matrixDelims{DelimBrace, DelimBrace, true, false}
	default: // "matrix", "smallmatrix"
		return matrixDelims{DelimParen, DelimParen, false, false}
	}
}

// matrixColGap is the number of blank columns between matrix columns.
const matrixColGap = 2

// casesColGap is the wider gap between a cases value and its condition.
const casesColGap = 2

// alignedColGap is the gap between the columns of an alignment environment. It
// is one column: the equations line up on the & (right-aligned left column, then
// a thin gap, then the left-aligned remainder) without the relation drifting far
// from its left-hand side.
const alignedColGap = 1

// cellAlign selects how a cell is padded to its column width.
type cellAlign int

const (
	alignCenter cellAlign = iota
	alignLeft
	alignRight
)

// columnAlign returns the alignment for column j of environment env. Matrix
// family columns are centered; cases is left-aligned; alignment environments
// (align/aligned/split/eqnarray) right-align the first column and left-align the
// rest so the rows line up on the relation at the &; gather/gathered center.
func columnAlign(env string, j int) cellAlign {
	switch {
	case env == "cases":
		return alignLeft
	case isGatheredEnv(env):
		return alignCenter
	case isAlignedEnv(env):
		if j == 0 {
			return alignRight
		}
		return alignLeft
	default: // matrix family
		return alignCenter
	}
}

// columnGap returns the inter-column gap for environment env.
func columnGap(env string) int {
	switch {
	case env == "cases":
		return casesColGap
	case isAlignedEnv(env):
		return alignedColGap
	default:
		return matrixColGap
	}
}

// layoutMatrix lays out an array environment as an aligned grid wrapped in the
// environment's delimiters. Each cell is laid out to a Box; columns are sized to
// their widest cell and rows to their tallest cell (baseline-aligned within the
// row). Rows are then stacked with the grid's vertical center as the baseline,
// and the whole grid is flanked by the auto-sized delimiters. "cases" is left-
// aligned with a single left brace; alignment environments (aligned/align/…)
// carry no delimiters and line their equations up on the & column.
func layoutMatrix(m *Matrix) Box {
	if len(m.Rows) == 0 {
		return EmptyBox()
	}
	// Lay out every cell and find the column count.
	cols := 0
	cells := make([][]Box, len(m.Rows))
	for i, row := range m.Rows {
		cells[i] = make([]Box, len(row))
		for j, c := range row {
			cells[i][j] = layout(c)
		}
		if len(row) > cols {
			cols = len(row)
		}
	}
	if cols == 0 {
		return EmptyBox()
	}

	// Per-column width = widest cell in that column.
	colWidth := make([]int, cols)
	for _, row := range cells {
		for j, b := range row {
			if b.Width > colWidth[j] {
				colWidth[j] = b.Width
			}
		}
	}

	gap := columnGap(m.Env)

	// Assemble each row into a single Box by hconcat of padded cells + gaps.
	rowBoxes := make([]Box, len(cells))
	for i, row := range cells {
		parts := make([]Box, 0, cols*2)
		for j := 0; j < cols; j++ {
			var cell Box
			if j < len(row) {
				cell = row[j]
			}
			parts = append(parts, padCell(cell, colWidth[j], columnAlign(m.Env, j)))
			if j < cols-1 {
				parts = append(parts, blankColumns(gap))
			}
		}
		rowBoxes[i] = HConcat(parts...)
	}

	// Stack the rows with one blank row between them and put the baseline on the
	// vertical center so the delimiters straddle the grid symmetrically.
	grid := stackRows(rowBoxes)

	dl := matrixDelimsFor(m.Env)
	h := grid.Height()
	if h < 1 {
		h = 1
	}
	parts := make([]Box, 0, 3)
	if dl.drawLeft {
		parts = append(parts, LeftDelim(dl.left, h, grid.Baseline))
		parts = append(parts, blankColumns(1))
	}
	parts = append(parts, grid)
	if dl.drawRight {
		parts = append(parts, blankColumns(1))
		parts = append(parts, RightDelim(dl.right, h, grid.Baseline))
	}
	return HConcat(parts...)
}

// padCell pads a laid-out cell to the target column width per align. The cell
// keeps its own baseline so rows align on baselines in HConcat.
func padCell(cell Box, width int, align cellAlign) Box {
	if cell.Height() == 0 {
		// An empty cell becomes a blank column of the target width, one row tall.
		return Box{Lines: []string{blankLine(width)}, Baseline: 0, Width: width}
	}
	if cell.Width >= width {
		return cell
	}
	lines := make([]string, len(cell.Lines))
	for i, l := range cell.Lines {
		switch align {
		case alignLeft:
			lines[i] = pad(l, width)
		case alignRight:
			lines[i] = padLeft(l, width)
		default:
			lines[i] = center(l, width)
		}
	}
	return Box{Lines: lines, Baseline: cell.Baseline, Width: width}
}

// padLeft left-pads line with spaces until its display width reaches want (a
// right-aligned cell). A line already at/over want is returned unchanged.
func padLeft(line string, want int) string {
	w := boxWidth(line)
	if w >= want {
		return line
	}
	return blankLine(want-w) + line
}

// blankColumns returns a one-row box of n blank columns (an inter-column gap).
func blankColumns(n int) Box {
	if n <= 0 {
		return EmptyBox()
	}
	return Box{Lines: []string{blankLine(n)}, Baseline: 0, Width: n}
}

// stackRows stacks row boxes top to bottom (no blank separator between them,
// matching a compact matrix) and puts the baseline on the middle row so a pair
// of tall delimiters brackets the grid symmetrically. Each row box may be
// multi-line (a cell containing a fraction); rows are centered to the common
// width first.
func stackRows(rows []Box) Box {
	if len(rows) == 0 {
		return EmptyBox()
	}
	width := 0
	for _, r := range rows {
		if r.Width > width {
			width = r.Width
		}
	}
	var lines []string
	baselineRow := 0
	// The grid baseline is the baseline row of the vertically middle row.
	mid := (len(rows) - 1) / 2
	for i, r := range rows {
		if i == mid {
			baselineRow = len(lines) + r.Baseline
		}
		for _, l := range r.Lines {
			lines = append(lines, center(l, width))
		}
	}
	return Box{Lines: lines, Baseline: baselineRow, Width: width}
}

// hasCombiningMark reports whether s contains any Unicode combining mark
// (U+0300–U+036F). The layout must never emit one (see the package doc); this
// is the guard render.go uses before returning a block.
func hasCombiningMark(s string) bool {
	for _, r := range s {
		if r >= 0x0300 && r <= 0x036F {
			return true
		}
	}
	return false
}
