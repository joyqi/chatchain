package mathtext

import (
	"errors"
	"fmt"
	"strings"
)

// This file is the LaTeX math parser for Phase 2: a pure function that turns a
// LaTeX math string into an abstract syntax tree (the AST node set below). It
// does NO rendering — the 2D layout engine (Stage A3+) consumes the tree. When
// the input falls outside the Tier-1 subset (an unknown macro, a malformed
// group, a Tier-3 construct such as \overbrace/\align/\phantom/tensors), Parse
// returns an error so the caller can fall back to CleanSource.
//
// The AST shape and the recursive-descent tokenizer/parser structure are
// modelled on the go-latex/latex project (BSD-3-Clause, archived): its
// scanner -> parser -> ast pipeline is the reference DESIGN. No go-latex code is
// imported — the node set and the descent were written from scratch for this
// package's Tier-1 subset, reusing this package's own symbol tables
// (symbols.go) and delimiter helpers (delim.go).

// maxParseDepth bounds the recursive descent so pathological or adversarial
// input (deeply nested braces/scripts) cannot blow the Go stack. Real math from
// models nests only a handful of levels; the guard is generous.
const maxParseDepth = 64

// maxMathInputRunes caps a display formula so worst-case O(n^2) layout stays
// well under a frame (~10ms); longer input falls back to cleaned source.
const maxMathInputRunes = 4000

// ErrUnsupported is returned (wrapped with context) when the input contains a
// construct outside the Tier-1 subset, signalling the caller to fall back to
// the cleaned linear source. Callers can test it with errors.Is.
var ErrUnsupported = errors.New("mathtext: unsupported LaTeX construct")

// Node is the sealed interface implemented by every AST node. The isNode marker
// keeps the set closed so a type switch in the layout engine is exhaustive.
type Node interface {
	isNode()
}

// Seq is a run of nodes concatenated left to right (the body of a group, a
// script argument, a matrix cell, …). An empty Seq represents an empty group.
type Seq struct {
	Items []Node
}

// Atom is a single visual unit already resolved to its display rune(s): a
// symbol (greek/operator from the tables), a digit, an identifier letter, or a
// punctuation/relation character. Text holds the runes to draw as-is; no
// further mapping is applied downstream.
type Atom struct {
	Text string
}

// Frac is a fraction \frac{Num}{Den} (and the tfrac/dfrac/cfrac variants).
type Frac struct {
	Num Node
	Den Node
}

// Sqrt is a radical \sqrt{Radicand}; Index is the optional degree from the
// \sqrt[n]{...} form (nil for a plain square root).
type Sqrt struct {
	Radicand Node
	Index    Node // optional; nil when absent
}

// Sup is a superscript Base^Exp with no subscript.
type Sup struct {
	Base Node
	Exp  Node
}

// Sub is a subscript Base_Sub with no superscript.
type Sub struct {
	Base Node
	Sub  Node
}

// SupSub is a base carrying both a superscript and a subscript (a^b_c or
// a_b^c). Either script is guaranteed non-nil for this node; a base with only
// one script uses Sup or Sub.
type SupSub struct {
	Base Node
	Sup  Node
	Sub  Node
}

// Delim is an auto-sized delimiter pair \left<L> ... \right<R> around Inner.
// Left and Right are the normalized delimiter strings ("(", ")", "[", "]",
// "{", "}", "|", "."). A "." marks an omitted (invisible) side.
type Delim struct {
	Left  string
	Right string
	Inner Node
}

// BigOp is a large operator with optional limits: \sum, \prod, \int, or \lim.
// Op is one of "sum", "prod", "int", "lim". Lower/Upper are the _/^ limits
// (either may be nil); Body is the operand that follows the operator, which may
// be nil when the operator is written bare. Word holds the upright name to
// draw for a word operator (\lim, \max, \det, …); it is "" for the glyph
// operators (\sum/\prod/\int), which the layout engine draws from a symbol.
type BigOp struct {
	Op    string // family: sum|prod|int|lim (tall shape + limit placement)
	Glyph string // the operator's own drawn glyph (union stays ⋃, not ∑)
	Word  string // upright name for word operators; "" for glyph operators
	Lower Node   // from _{...}; may be nil
	Upper Node   // from ^{...}; may be nil
	Body  Node   // the following operand; may be nil
}

// Matrix is an array environment. Env is the environment name ("matrix",
// "pmatrix", "bmatrix", "vmatrix", "Vmatrix", "cases"). Rows holds the cells,
// row-major; a ragged final row is allowed. For cases the two columns are the
// value and its condition.
type Matrix struct {
	Env  string
	Rows [][]Node
}

// Text is the literal prose of a \text{...} argument, drawn verbatim (no math
// mapping, CJK-safe downstream via uniseg width).
type Text struct {
	S string
}

// Accent is a single-argument accent (\hat \bar \vec \tilde \dot \ddot
// \overline) over Base. Kind selects the drawn glyph row placed ABOVE the base
// (never a combining mark): the layout draws it as its own row centered over the
// base box, exactly like the sqrt vinculum.
type Accent struct {
	Kind AccentKind
	Base Node
}

func (*Seq) isNode()    {}
func (*Atom) isNode()   {}
func (*Frac) isNode()   {}
func (*Sqrt) isNode()   {}
func (*Sup) isNode()    {}
func (*Sub) isNode()    {}
func (*SupSub) isNode() {}
func (*Delim) isNode()  {}
func (*BigOp) isNode()  {}
func (*Matrix) isNode() {}
func (*Text) isNode()   {}
func (*Accent) isNode() {}

// Parse turns a LaTeX math string into an AST. The input may still carry a
// surrounding delimiter pair ($...$, $$...$$, \(...\), \[...\]); it is stripped
// first. An empty body, an unknown macro, a malformed group, or any Tier-3
// construct returns an error (wrapping ErrUnsupported) so the caller falls back
// to CleanSource. Parse is pure: same input, same tree, no I/O.
func Parse(latex string) (Node, error) {
	body := StripDelimiters(latex)
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("%w: empty math body", ErrUnsupported)
	}
	// Bound the input: layout is O(n2) in atom count (HConcat accumulates), and
	// Render2D runs synchronously on the streaming render path with no timeout.
	// A pathologically long single formula (hallucinated or adversarial) would
	// otherwise freeze the REPL for seconds; past the cap we fall back to the
	// O(n) cleaned-source rendering instead.
	if len([]rune(body)) > maxMathInputRunes {
		return nil, fmt.Errorf("%w: formula too long (%d runes)", ErrUnsupported, len([]rune(body)))
	}
	p := &parser{src: []rune(body)}
	node, err := p.parseExpr(0)
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos < len(p.src) {
		// Leftover input means a stray close brace/bracket or a \right without a
		// \left, etc. — treat as malformed so the caller falls back.
		return nil, fmt.Errorf("%w: unexpected %q at end", ErrUnsupported, string(p.src[p.pos]))
	}
	return node, nil
}

// parser holds the rune scan state for the recursive descent.
type parser struct {
	src []rune
	pos int
}

// parseExpr parses a run of atoms up to a stopper (end of input, a closing
// brace/bracket, an & or \\ at the current matrix level, or a \right). It binds
// scripts (^ and _) to the immediately preceding atom and lifts scripts on a
// big operator into that operator's limits. depth guards recursion.
func (p *parser) parseExpr(depth int) (Node, error) {
	if depth > maxParseDepth {
		return nil, fmt.Errorf("%w: nesting too deep", ErrUnsupported)
	}
	var items []Node
	for {
		p.skipSpace()
		if p.atStopper() {
			break
		}
		atom, err := p.parseAtom(depth)
		if err != nil {
			return nil, err
		}
		if atom == nil {
			// A layout/spacing macro produced nothing; keep scanning.
			continue
		}
		// A big operator absorbs following _/^ as limits rather than scripts.
		if op, ok := atom.(*BigOp); ok {
			if err := p.attachLimits(op, depth); err != nil {
				return nil, err
			}
			items = append(items, op)
			continue
		}
		// Attach any ^ / _ scripts to this atom.
		scripted, err := p.parseScripts(atom, depth)
		if err != nil {
			return nil, err
		}
		items = append(items, scripted)
	}
	switch len(items) {
	case 0:
		return &Seq{}, nil
	case 1:
		return items[0], nil
	default:
		return &Seq{Items: items}, nil
	}
}

// atStopper reports whether the scanner sits at a token that ends the current
// expression run: end of input, a group/array delimiter, or a \right.
func (p *parser) atStopper() bool {
	if p.pos >= len(p.src) {
		return true
	}
	switch p.src[p.pos] {
	case '}', ']', '&':
		return true
	case '\\':
		if p.hasCommandAt(p.pos, "right") {
			return true
		}
		if p.hasRowBreakAt(p.pos) {
			return true
		}
		if p.hasCommandAt(p.pos, "end") {
			return true
		}
	}
	return false
}

// parseScripts binds a trailing ^ and/or _ to base, producing Sup, Sub, or
// SupSub. LaTeX forbids two of the same script on one base (x^a^b); that is
// rejected as malformed. The scripts may appear in either order.
func (p *parser) parseScripts(base Node, depth int) (Node, error) {
	var sup, sub Node
	for {
		p.skipSpace()
		if p.pos >= len(p.src) {
			break
		}
		c := p.src[p.pos]
		if c != '^' && c != '_' {
			break
		}
		p.pos++
		arg, err := p.parseScriptArg(depth)
		if err != nil {
			return nil, err
		}
		if c == '^' {
			if sup != nil {
				return nil, fmt.Errorf("%w: double superscript", ErrUnsupported)
			}
			sup = arg
		} else {
			if sub != nil {
				return nil, fmt.Errorf("%w: double subscript", ErrUnsupported)
			}
			sub = arg
		}
	}
	switch {
	case sup != nil && sub != nil:
		return &SupSub{Base: base, Sup: sup, Sub: sub}, nil
	case sup != nil:
		return &Sup{Base: base, Exp: sup}, nil
	case sub != nil:
		return &Sub{Base: base, Sub: sub}, nil
	default:
		return base, nil
	}
}

// parseScriptArg reads the argument of a ^ or _: a braced group, or a single
// token (one atom, without further scripts of its own).
func (p *parser) parseScriptArg(depth int) (Node, error) {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return nil, fmt.Errorf("%w: missing script argument", ErrUnsupported)
	}
	if p.src[p.pos] == '{' {
		return p.parseGroup(depth)
	}
	return p.parseAtom(depth + 1)
}

// attachLimits reads the _/^ limits that follow a big operator (in either
// order) and, for \sum/\prod/\int/\lim with a following operand, the body. The
// operand is a single atom-with-scripts so "\sum_{i} i^2" binds the ^2 to i,
// not to the sum.
func (p *parser) attachLimits(op *BigOp, depth int) error {
	for {
		p.skipSpace()
		if p.pos >= len(p.src) {
			return nil
		}
		c := p.src[p.pos]
		if c != '^' && c != '_' {
			break
		}
		p.pos++
		arg, err := p.parseScriptArg(depth)
		if err != nil {
			return err
		}
		if c == '^' {
			if op.Upper != nil {
				return fmt.Errorf("%w: double upper limit", ErrUnsupported)
			}
			op.Upper = arg
		} else {
			if op.Lower != nil {
				return fmt.Errorf("%w: double lower limit", ErrUnsupported)
			}
			op.Lower = arg
		}
	}
	return nil
}

// parseAtom parses one leading atom: a command, a group, a delimiter pair, a
// number run, an identifier letter, or a single ordinary/relation character.
// It returns (nil, nil) for a layout/spacing macro that yields no node.
func (p *parser) parseAtom(depth int) (Node, error) {
	if depth > maxParseDepth {
		return nil, fmt.Errorf("%w: nesting too deep", ErrUnsupported)
	}
	if p.pos >= len(p.src) {
		return nil, fmt.Errorf("%w: unexpected end of input", ErrUnsupported)
	}
	c := p.src[p.pos]
	switch {
	case c == '\\':
		return p.parseCommand(depth)
	case c == '{':
		return p.parseGroup(depth)
	case c >= '0' && c <= '9':
		return p.parseNumber(), nil
	case isASCIILetter(c):
		p.pos++
		return &Atom{Text: string(c)}, nil
	case c == '^' || c == '_':
		// A script with no base is malformed.
		return nil, fmt.Errorf("%w: script without base", ErrUnsupported)
	case c == '}' || c == ']' || c == '&':
		return nil, fmt.Errorf("%w: unexpected %q", ErrUnsupported, string(c))
	default:
		// Ordinary/relation/operator character: parentheses, + - = < > | etc.
		p.pos++
		return &Atom{Text: string(c)}, nil
	}
}

// parseNumber consumes a run of digits (and an interior decimal point) into a
// single Atom so "128" is one visual unit rather than three.
func (p *parser) parseNumber() Node {
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if (c >= '0' && c <= '9') || c == '.' {
			p.pos++
			continue
		}
		break
	}
	return &Atom{Text: string(p.src[start:p.pos])}
}

// parseGroup parses a braced group {...} into a Seq (or its single child).
func (p *parser) parseGroup(depth int) (Node, error) {
	if p.src[p.pos] != '{' {
		return nil, fmt.Errorf("%w: expected '{'", ErrUnsupported)
	}
	p.pos++ // consume '{'
	inner, err := p.parseExpr(depth + 1)
	if err != nil {
		return nil, err
	}
	if p.pos >= len(p.src) || p.src[p.pos] != '}' {
		return nil, fmt.Errorf("%w: unbalanced '{'", ErrUnsupported)
	}
	p.pos++ // consume '}'
	return inner, nil
}

// parseCommand parses a backslash sequence: an escaped literal, a math macro
// with structure (\frac, \sqrt, \left, \begin, \text, big operators), a symbol
// macro from the tables, or a droppable layout/spacing macro. Unknown macros
// return an error so the caller falls back.
func (p *parser) parseCommand(depth int) (Node, error) {
	// Escaped punctuation: \{ \} \$ \% \& \# \_ -> the literal character.
	if p.pos+1 < len(p.src) {
		switch p.src[p.pos+1] {
		case '{', '}', '$', '%', '&', '#', '_', ' ':
			ch := p.src[p.pos+1]
			p.pos += 2
			if ch == ' ' {
				return nil, nil // explicit control space: drop
			}
			return &Atom{Text: string(ch)}, nil
		}
	}
	name, spaced := p.scanCommandName()
	if name == "" {
		return nil, fmt.Errorf("%w: lone backslash", ErrUnsupported)
	}
	_ = spaced

	// Control symbols (a backslash followed by one non-letter) that map to a
	// glyph: \| is the norm bar ‖, \, was already handled as a space above.
	switch name {
	case "|":
		return &Atom{Text: "‖"}, nil // U+2016 double vertical line (norm)
	case "backslash":
		return &Atom{Text: "\\"}, nil
	}

	switch name {
	case "frac", "tfrac", "dfrac", "cfrac":
		return p.parseFrac(depth)
	case "sqrt":
		return p.parseSqrt(depth)
	case "left":
		return p.parseDelim(depth)
	case "right":
		// A \right with no matching \left reaching here is malformed.
		return nil, fmt.Errorf("%w: \\right without \\left", ErrUnsupported)
	case "begin":
		return p.parseEnv(depth)
	case "end":
		return nil, fmt.Errorf("%w: \\end without \\begin", ErrUnsupported)
	case "text", "textrm", "textbf", "textit", "textsf", "texttt", "mbox":
		return p.parseText()
	case "sum", "prod", "int", "iint", "iiint", "oint", "coprod", "bigcup",
		"bigcap", "bigoplus", "bigotimes", "bigwedge", "bigvee":
		return &BigOp{Op: bigOpKind(name), Glyph: bigOpSingleGlyph(name)}, nil
	case "lim", "limsup", "liminf", "max", "min", "sup", "inf", "det", "gcd",
		"arg", "deg", "dim", "ker", "hom":
		return &BigOp{Op: "lim", Word: wordOpName(name)}, nil
	}

	// Accent macros (\hat \bar \vec \tilde \dot \ddot \overline) take a single
	// argument and draw a glyph row above it.
	if kind, ok := accentKind(name); ok {
		base, err := p.parseRequiredArg(depth)
		if err != nil {
			return nil, err
		}
		return &Accent{Kind: kind, Base: base}, nil
	}

	// Math-font macros (\mathbf \mathcal \mathbb \mathrm …) take a single group
	// argument and map its letters to the Unicode math-alphanumeric variant (or
	// degrade to plain), so they never trigger a fallback.
	if style, ok := mathFontStyle(name); ok {
		arg, err := p.parseRequiredArg(depth)
		if err != nil {
			return nil, err
		}
		return applyFont(style, arg), nil
	}

	// Named function operators (\sin, \cos, \log, …) render as their upright
	// name; treat them as a multi-letter Atom.
	if isFuncName(name) {
		return &Atom{Text: name}, nil
	}

	// Layout / spacing / style macros carry no glyph: drop them (and their
	// no-arg selves). They must not error — they are common in real input.
	if isLayoutMacro(name) {
		return nil, nil
	}

	// Symbol macro from the greek/operator tables.
	if r, ok := symbolRune(name); ok {
		return &Atom{Text: string(r)}, nil
	}

	// Anything else is outside the Tier-1 subset: fall back.
	return nil, fmt.Errorf("%w: unknown macro \\%s", ErrUnsupported, name)
}

// scanCommandName reads a control word (a run of ASCII letters) or a control
// symbol (a single non-letter) starting just after the backslash at p.pos. It
// advances p.pos past the name and any swallowed trailing spaces, returning the
// name and whether a trailing space was swallowed.
func (p *parser) scanCommandName() (name string, spaced bool) {
	i := p.pos + 1 // skip backslash
	if i >= len(p.src) {
		p.pos = i
		return "", false
	}
	if !isASCIILetter(p.src[i]) {
		p.pos = i + 1
		return string(p.src[i]), false
	}
	j := i
	for j < len(p.src) && isASCIILetter(p.src[j]) {
		j++
	}
	name = string(p.src[i:j])
	for j < len(p.src) && p.src[j] == ' ' {
		j++
		spaced = true
	}
	p.pos = j
	return name, spaced
}

// applyFont rewrites every Atom/Text glyph inside a parsed math-font argument to
// its math-alphanumeric variant for style (StylePlain leaves it untouched). It
// walks the subtree so \mathbf{x_i} keeps its script structure while mapping the
// letters, and any unmapped rune degrades to itself — so the transform is total
// and never fails. Big operators, delimiters, and matrices are passed through
// structurally (their own atoms are still rewritten).
func applyFont(style MathStyle, n Node) Node {
	if style == StylePlain {
		return n
	}
	switch v := n.(type) {
	case nil:
		return nil
	case *Atom:
		return &Atom{Text: applyMathFont(style, v.Text)}
	case *Text:
		return &Text{S: applyMathFont(style, v.S)}
	case *Seq:
		items := make([]Node, len(v.Items))
		for i, it := range v.Items {
			items[i] = applyFont(style, it)
		}
		return &Seq{Items: items}
	case *Frac:
		return &Frac{Num: applyFont(style, v.Num), Den: applyFont(style, v.Den)}
	case *Sqrt:
		return &Sqrt{Radicand: applyFont(style, v.Radicand), Index: applyFont(style, v.Index)}
	case *Sup:
		return &Sup{Base: applyFont(style, v.Base), Exp: applyFont(style, v.Exp)}
	case *Sub:
		return &Sub{Base: applyFont(style, v.Base), Sub: applyFont(style, v.Sub)}
	case *SupSub:
		return &SupSub{Base: applyFont(style, v.Base), Sup: applyFont(style, v.Sup), Sub: applyFont(style, v.Sub)}
	case *Accent:
		return &Accent{Kind: v.Kind, Base: applyFont(style, v.Base)}
	case *Delim:
		return &Delim{Left: v.Left, Right: v.Right, Inner: applyFont(style, v.Inner)}
	default:
		// BigOp / Matrix: an atypical font argument; leave it structurally intact
		// rather than descend (the common case is a short letter group).
		return n
	}
}

// parseFrac parses the two brace arguments of \frac{num}{den}.
func (p *parser) parseFrac(depth int) (Node, error) {
	num, err := p.parseRequiredArg(depth)
	if err != nil {
		return nil, err
	}
	den, err := p.parseRequiredArg(depth)
	if err != nil {
		return nil, err
	}
	return &Frac{Num: num, Den: den}, nil
}

// parseSqrt parses \sqrt{x} and the optional-degree form \sqrt[n]{x}.
func (p *parser) parseSqrt(depth int) (Node, error) {
	var index Node
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == '[' {
		idx, err := p.parseBracketArg(depth)
		if err != nil {
			return nil, err
		}
		index = idx
	}
	rad, err := p.parseRequiredArg(depth)
	if err != nil {
		return nil, err
	}
	return &Sqrt{Radicand: rad, Index: index}, nil
}

// parseDelim parses \left<D> ... \right<D> into a Delim. The delimiter after
// \left and \right is a single char ( ) [ ] { } | or "." (invisible). The
// inner run is parsed up to the matching \right at this depth.
func (p *parser) parseDelim(depth int) (Node, error) {
	left, err := p.readDelimSymbol()
	if err != nil {
		return nil, err
	}
	inner, err := p.parseExpr(depth + 1)
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if !p.hasCommandAt(p.pos, "right") {
		return nil, fmt.Errorf("%w: \\left without matching \\right", ErrUnsupported)
	}
	p.consumeCommand("right")
	right, err := p.readDelimSymbol()
	if err != nil {
		return nil, err
	}
	return &Delim{Left: left, Right: right, Inner: inner}, nil
}

// readDelimSymbol reads the delimiter that follows \left or \right: a bare
// ( ) [ ] | . character, an escaped \{ \} \| \langle \rangle, or a "." for an
// omitted side. It returns the normalized delimiter string.
func (p *parser) readDelimSymbol() (string, error) {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return "", fmt.Errorf("%w: missing delimiter after \\left/\\right", ErrUnsupported)
	}
	c := p.src[p.pos]
	if c == '\\' {
		name, _ := p.scanCommandName()
		switch name {
		case "{":
			return "{", nil
		case "}":
			return "}", nil
		case "|", "vert", "Vert":
			return "|", nil
		case "langle":
			return "⟨", nil
		case "rangle":
			return "⟩", nil
		case "lfloor":
			return "⌊", nil
		case "rfloor":
			return "⌋", nil
		case "lceil":
			return "⌈", nil
		case "rceil":
			return "⌉", nil
		}
		return "", fmt.Errorf("%w: unsupported delimiter \\%s", ErrUnsupported, name)
	}
	switch c {
	case '(', ')', '[', ']', '|', '/', '.':
		p.pos++
		return string(c), nil
	}
	return "", fmt.Errorf("%w: unsupported delimiter %q", ErrUnsupported, string(c))
}

// parseEnv parses \begin{env} ... \end{env} into a Matrix. The supported
// environments are matrix/pmatrix/bmatrix/Bmatrix/vmatrix/Vmatrix and cases.
// Cells are separated by & and rows by \\; a trailing empty row is dropped.
func (p *parser) parseEnv(depth int) (Node, error) {
	env, err := p.readEnvName()
	if err != nil {
		return nil, err
	}
	if !isMatrixEnv(env) && !isAlignedEnv(env) {
		return nil, fmt.Errorf("%w: unsupported environment %q", ErrUnsupported, env)
	}
	var rows [][]Node
	var row []Node
	for {
		cell, err := p.parseExpr(depth + 1)
		if err != nil {
			return nil, err
		}
		row = append(row, cell)
		p.skipSpace()
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("%w: %s environment not closed", ErrUnsupported, env)
		}
		if p.src[p.pos] == '&' {
			p.pos++
			continue
		}
		if p.hasRowBreakAt(p.pos) {
			p.pos += 2 // consume "\\"
			p.skipRowBreakOptions()
			rows = append(rows, row)
			row = nil
			continue
		}
		if p.hasCommandAt(p.pos, "end") {
			rows = append(rows, row)
			p.consumeCommand("end")
			closeName, err := p.readEnvName()
			if err != nil {
				return nil, err
			}
			if closeName != env {
				return nil, fmt.Errorf("%w: \\begin{%s} closed by \\end{%s}", ErrUnsupported, env, closeName)
			}
			break
		}
		return nil, fmt.Errorf("%w: malformed %s environment", ErrUnsupported, env)
	}
	rows = dropTrailingEmptyRow(rows)
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: empty %s environment", ErrUnsupported, env)
	}
	return &Matrix{Env: env, Rows: rows}, nil
}

// skipRowBreakOptions drops the optional spacing argument that may follow a "\\"
// row break in an alignment environment: a starred break ("\\*") and/or a
// bracketed length ("\\[4pt]", "\\[1ex]"). LaTeX allows this after every row
// break; without stripping it the "[" would parse as a stray token and the
// environment would fall back.
func (p *parser) skipRowBreakOptions() {
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == '*' {
		p.pos++
		p.skipSpace()
	}
	if p.pos < len(p.src) && p.src[p.pos] == '[' {
		for p.pos < len(p.src) && p.src[p.pos] != ']' {
			p.pos++
		}
		if p.pos < len(p.src) {
			p.pos++ // consume ']'
		}
	}
}

// dropTrailingEmptyRow removes a final row that consists of a single empty
// cell, which results from a trailing "\\" before \end.
func dropTrailingEmptyRow(rows [][]Node) [][]Node {
	if n := len(rows); n > 0 {
		last := rows[n-1]
		if len(last) == 1 && isEmptyNode(last[0]) {
			return rows[:n-1]
		}
	}
	return rows
}

// isEmptyNode reports whether n is an empty Seq (an empty cell/group).
func isEmptyNode(n Node) bool {
	s, ok := n.(*Seq)
	return ok && len(s.Items) == 0
}

// readEnvName reads a {name} group following \begin or \end, returning the raw
// environment name. The name may carry a trailing "*" (starred variant), which
// is stripped.
func (p *parser) readEnvName() (string, error) {
	p.skipSpace()
	if p.pos >= len(p.src) || p.src[p.pos] != '{' {
		return "", fmt.Errorf("%w: expected {name} after \\begin/\\end", ErrUnsupported)
	}
	p.pos++ // consume '{'
	start := p.pos
	for p.pos < len(p.src) && p.src[p.pos] != '}' {
		p.pos++
	}
	if p.pos >= len(p.src) {
		return "", fmt.Errorf("%w: unterminated environment name", ErrUnsupported)
	}
	name := string(p.src[start:p.pos])
	p.pos++ // consume '}'
	return strings.TrimSuffix(name, "*"), nil
}

// parseText parses \text{...} (and its font-variant aliases). The argument is
// captured as literal prose — brace-balanced, backslash-escape aware — into a
// Text node; no math mapping is applied.
func (p *parser) parseText() (Node, error) {
	p.skipSpace()
	if p.pos >= len(p.src) || p.src[p.pos] != '{' {
		return nil, fmt.Errorf("%w: \\text without a braced argument", ErrUnsupported)
	}
	p.pos++ // consume '{'
	var b strings.Builder
	depth := 1
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch c {
		case '\\':
			// Keep an escaped char literally (drop the backslash); a lone trailing
			// backslash is dropped.
			if p.pos+1 < len(p.src) {
				b.WriteRune(p.src[p.pos+1])
				p.pos += 2
				continue
			}
			p.pos++
		case '{':
			depth++
			b.WriteRune(c)
			p.pos++
		case '}':
			depth--
			if depth == 0 {
				p.pos++ // consume closing '}'
				return &Text{S: b.String()}, nil
			}
			b.WriteRune(c)
			p.pos++
		default:
			b.WriteRune(c)
			p.pos++
		}
	}
	return nil, fmt.Errorf("%w: unterminated \\text argument", ErrUnsupported)
}

// parseRequiredArg reads a mandatory macro argument: a braced group (its inner
// expression) or a single atom. It errors if nothing is present.
func (p *parser) parseRequiredArg(depth int) (Node, error) {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return nil, fmt.Errorf("%w: missing required argument", ErrUnsupported)
	}
	if p.src[p.pos] == '{' {
		return p.parseGroup(depth)
	}
	if p.src[p.pos] == '}' || p.src[p.pos] == '&' || p.src[p.pos] == ']' {
		return nil, fmt.Errorf("%w: missing required argument", ErrUnsupported)
	}
	return p.parseAtom(depth + 1)
}

// parseBracketArg reads an optional [n] argument (used by \sqrt[n]{}). The
// inner text is parsed as an expression up to the matching ']'.
func (p *parser) parseBracketArg(depth int) (Node, error) {
	p.pos++ // consume '['
	inner, err := p.parseExpr(depth + 1)
	if err != nil {
		return nil, err
	}
	if p.pos >= len(p.src) || p.src[p.pos] != ']' {
		return nil, fmt.Errorf("%w: unbalanced '['", ErrUnsupported)
	}
	p.pos++ // consume ']'
	return inner, nil
}

// skipSpace advances over ASCII spaces and tabs (LaTeX ignores them between
// tokens in math mode). Newlines are also treated as inter-token space.
func (p *parser) skipSpace() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

// hasCommandAt reports whether the rune at index i begins the control word
// "\name" (a backslash followed by exactly that letter run, not a longer one).
func (p *parser) hasCommandAt(i int, name string) bool {
	if i >= len(p.src) || p.src[i] != '\\' {
		return false
	}
	nm := []rune(name)
	if i+1+len(nm) > len(p.src) {
		return false
	}
	for k, r := range nm {
		if p.src[i+1+k] != r {
			return false
		}
	}
	// Ensure it is not a longer control word (e.g. "endfoo" vs "end").
	after := i + 1 + len(nm)
	if after < len(p.src) && isASCIILetter(p.src[after]) {
		return false
	}
	return true
}

// hasRowBreakAt reports whether index i is a "\\" row separator.
func (p *parser) hasRowBreakAt(i int) bool {
	return i+1 < len(p.src) && p.src[i] == '\\' && p.src[i+1] == '\\'
}

// consumeCommand advances p.pos past "\name" (and any swallowed trailing
// spaces). It assumes hasCommandAt already matched.
func (p *parser) consumeCommand(name string) {
	p.pos += 1 + len([]rune(name))
	for p.pos < len(p.src) && p.src[p.pos] == ' ' {
		p.pos++
	}
}
