// Package expr implements a tiny, safe boolean expression language used by
// routing rules to express custom routing conditions over a fixed set of
// request variables.
//
// Grammar (recursive descent, no external deps):
//
//	expr    := or
//	or      := and ( "||" and )*
//	and      := not ( "&&" not )*
//	not     := "!" not | comparison
//	comparison := primary ( ("=="|"!="|"<"|"<="|">"|">=") primary )?
//	primary := "(" expr ")" | number | string | bool | ident
//
// Variables (the only identifiers allowed) are validated at Compile time:
//   - w      int    (probe: 1 if next turn likely has a write/tool call, else 0)
//   - t      int    (probe: predicted output token length)
//   - tokens int    (estimated prompt tokens)
//   - group  string (caller's routing group)
//   - model  string (requested model name)
//
// Compile reports the set of variables a program references (so the engine can
// decide whether it must invoke the probe). Eval runs against a Vars map.
//
// Safety: the grammar is fixed and closed — there are no function calls, no
// field access, no indexing, and only the five whitelisted identifiers are
// accepted, so a stored expression can never reach arbitrary code or data.
package expr

import (
	"fmt"
	"strconv"
	"strings"
)

// Known variable names. Compile rejects any other identifier (except the bare
// literals true/false, handled by the lexer).
const (
	VarW      = "w"
	VarT      = "t"
	VarTokens = "tokens"
	VarGroup  = "group"
	VarModel  = "model"
)

var knownVars = map[string]bool{
	VarW: true, VarT: true, VarTokens: true, VarGroup: true, VarModel: true,
}

// numericVars are integer-valued; the rest are string-valued. Used for type
// checking at Compile time.
var numericVars = map[string]bool{VarW: true, VarT: true, VarTokens: true}

// Vars holds the runtime values for an evaluation. Numeric vars come from Int,
// string vars from Str; absent entries default to 0 / "".
type Vars struct {
	Int map[string]int
	Str map[string]string
}

// Program is a compiled expression: its root node plus the set of variable names
// it references.
type Program struct {
	root node
	// Refs is the set of variable names the program references.
	Refs map[string]bool
}

// References reports whether the program uses the named variable.
func (p *Program) References(name string) bool { return p != nil && p.Refs[name] }

// Eval evaluates the compiled program against v, returning the boolean result.
// A program is required to evaluate to a bool at the top level; Compile already
// guaranteed that, so Eval never errors.
func (p *Program) Eval(v Vars) bool {
	if p == nil || p.root == nil {
		return true // empty expression = unconstrained (always matches)
	}
	val := p.root.eval(v)
	b, _ := val.(bool)
	return b
}

// Compile parses and type-checks src. An empty/blank src compiles to a nil
// program that always matches (Eval returns true). A syntax or type error is
// returned with a human-readable message for the rule editor.
func Compile(src string) (*Program, error) {
	if strings.TrimSpace(src) == "" {
		return &Program{root: nil, Refs: map[string]bool{}}, nil
	}
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	root, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if !p.atEnd() {
		return nil, fmt.Errorf("unexpected %q at position %d", p.cur().text, p.cur().pos)
	}
	// The top-level expression must be boolean-typed.
	if root.typ() != typeBool {
		return nil, fmt.Errorf("expression must be a boolean condition (e.g. w == 1), got a %s value", root.typ())
	}
	refs := map[string]bool{}
	root.collectRefs(refs)
	return &Program{root: root, Refs: refs}, nil
}

// --- value types ----------------------------------------------------------

type valType int

const (
	typeBool valType = iota
	typeInt
	typeStr
)

func (vt valType) String() string {
	switch vt {
	case typeBool:
		return "boolean"
	case typeInt:
		return "number"
	default:
		return "string"
	}
}

// --- AST ------------------------------------------------------------------

type node interface {
	eval(v Vars) any
	typ() valType
	collectRefs(into map[string]bool)
}

type boolLit struct{ b bool }

func (n boolLit) eval(Vars) any               { return n.b }
func (n boolLit) typ() valType                { return typeBool }
func (n boolLit) collectRefs(map[string]bool) {}

type intLit struct{ n int }

func (n intLit) eval(Vars) any               { return n.n }
func (n intLit) typ() valType                { return typeInt }
func (n intLit) collectRefs(map[string]bool) {}

type strLit struct{ s string }

func (n strLit) eval(Vars) any               { return n.s }
func (n strLit) typ() valType                { return typeStr }
func (n strLit) collectRefs(map[string]bool) {}

type varRef struct {
	name string
	vt   valType
}

func (n varRef) eval(v Vars) any {
	if n.vt == typeInt {
		if v.Int != nil {
			return v.Int[n.name]
		}
		return 0
	}
	if v.Str != nil {
		return v.Str[n.name]
	}
	return ""
}
func (n varRef) typ() valType                     { return n.vt }
func (n varRef) collectRefs(into map[string]bool) { into[n.name] = true }

type notNode struct{ x node }

func (n notNode) eval(v Vars) any { b, _ := n.x.eval(v).(bool); return !b }
func (n notNode) typ() valType    { return typeBool }
func (n notNode) collectRefs(into map[string]bool) {
	n.x.collectRefs(into)
}

type logicNode struct {
	op   string // "&&" | "||"
	l, r node
}

func (n logicNode) eval(v Vars) any {
	lb, _ := n.l.eval(v).(bool)
	if n.op == "&&" {
		if !lb {
			return false
		}
		rb, _ := n.r.eval(v).(bool)
		return rb
	}
	// "||"
	if lb {
		return true
	}
	rb, _ := n.r.eval(v).(bool)
	return rb
}
func (n logicNode) typ() valType { return typeBool }
func (n logicNode) collectRefs(into map[string]bool) {
	n.l.collectRefs(into)
	n.r.collectRefs(into)
}

type cmpNode struct {
	op   string
	l, r node
	// numeric reports whether the comparison is over ints (else strings).
	numeric bool
}

func (n cmpNode) eval(v Vars) any {
	if n.numeric {
		li := toInt(n.l.eval(v))
		ri := toInt(n.r.eval(v))
		switch n.op {
		case "==":
			return li == ri
		case "!=":
			return li != ri
		case "<":
			return li < ri
		case "<=":
			return li <= ri
		case ">":
			return li > ri
		case ">=":
			return li >= ri
		}
		return false
	}
	ls := toStr(n.l.eval(v))
	rs := toStr(n.r.eval(v))
	switch n.op {
	case "==":
		return ls == rs
	case "!=":
		return ls != rs
	case "<":
		return ls < rs
	case "<=":
		return ls <= rs
	case ">":
		return ls > rs
	case ">=":
		return ls >= rs
	}
	return false
}
func (n cmpNode) typ() valType { return typeBool }
func (n cmpNode) collectRefs(into map[string]bool) {
	n.l.collectRefs(into)
	n.r.collectRefs(into)
}

func toInt(v any) int {
	if i, ok := v.(int); ok {
		return i
	}
	return 0
}
func toStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// --- lexer ----------------------------------------------------------------

type tokKind int

const (
	tkEOF tokKind = iota
	tkInt
	tkStr
	tkBool
	tkIdent
	tkOp // && || ! == != < <= > >=
	tkLParen
	tkRParen
)

type token struct {
	kind tokKind
	text string
	pos  int
	ival int
	bval bool
}

func lex(src string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(':
			toks = append(toks, token{kind: tkLParen, text: "(", pos: i})
			i++
		case c == ')':
			toks = append(toks, token{kind: tkRParen, text: ")", pos: i})
			i++
		case c == '"' || c == '\'':
			quote := c
			j := i + 1
			var sb strings.Builder
			for j < len(src) && src[j] != quote {
				if src[j] == '\\' && j+1 < len(src) {
					j++
				}
				sb.WriteByte(src[j])
				j++
			}
			if j >= len(src) {
				return nil, fmt.Errorf("unterminated string starting at position %d", i)
			}
			toks = append(toks, token{kind: tkStr, text: sb.String(), pos: i})
			i = j + 1
		case c == '&' || c == '|':
			if i+1 < len(src) && src[i+1] == c {
				toks = append(toks, token{kind: tkOp, text: string([]byte{c, c}), pos: i})
				i += 2
			} else {
				return nil, fmt.Errorf("unexpected %q at position %d (did you mean %q?)", string(c), i, string([]byte{c, c}))
			}
		case c == '=' || c == '!' || c == '<' || c == '>':
			if i+1 < len(src) && src[i+1] == '=' {
				toks = append(toks, token{kind: tkOp, text: string(c) + "=", pos: i})
				i += 2
			} else if c == '!' {
				toks = append(toks, token{kind: tkOp, text: "!", pos: i})
				i++
			} else if c == '<' || c == '>' {
				toks = append(toks, token{kind: tkOp, text: string(c), pos: i})
				i++
			} else {
				return nil, fmt.Errorf("unexpected %q at position %d (use == for equality)", string(c), i)
			}
		case c >= '0' && c <= '9':
			j := i
			for j < len(src) && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			n, _ := strconv.Atoi(src[i:j])
			toks = append(toks, token{kind: tkInt, text: src[i:j], pos: i, ival: n})
			i = j
		case isIdentStart(c):
			j := i
			for j < len(src) && isIdentPart(src[j]) {
				j++
			}
			word := src[i:j]
			switch word {
			case "true":
				toks = append(toks, token{kind: tkBool, text: word, pos: i, bval: true})
			case "false":
				toks = append(toks, token{kind: tkBool, text: word, pos: i, bval: false})
			default:
				toks = append(toks, token{kind: tkIdent, text: word, pos: i})
			}
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q at position %d", string(c), i)
		}
	}
	toks = append(toks, token{kind: tkEOF, text: "", pos: len(src)})
	return toks, nil
}

func isIdentStart(c byte) bool { return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isIdentPart(c byte) bool  { return isIdentStart(c) || (c >= '0' && c <= '9') }

// --- parser ---------------------------------------------------------------

type parser struct {
	toks []token
	i    int
}

func (p *parser) cur() token  { return p.toks[p.i] }
func (p *parser) atEnd() bool { return p.cur().kind == tkEOF }
func (p *parser) advance() token {
	t := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}

func (p *parser) parseExpr() (node, error) { return p.parseOr() }

func (p *parser) parseOr() (node, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.cur().kind == tkOp && p.cur().text == "||" {
		p.advance()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		if left.typ() != typeBool || right.typ() != typeBool {
			return nil, fmt.Errorf("|| requires boolean operands")
		}
		left = logicNode{op: "||", l: left, r: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (node, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.cur().kind == tkOp && p.cur().text == "&&" {
		p.advance()
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		if left.typ() != typeBool || right.typ() != typeBool {
			return nil, fmt.Errorf("&& requires boolean operands")
		}
		left = logicNode{op: "&&", l: left, r: right}
	}
	return left, nil
}

func (p *parser) parseNot() (node, error) {
	if p.cur().kind == tkOp && p.cur().text == "!" {
		p.advance()
		x, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		if x.typ() != typeBool {
			return nil, fmt.Errorf("! requires a boolean operand")
		}
		return notNode{x: x}, nil
	}
	return p.parseComparison()
}

func (p *parser) parseComparison() (node, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	if p.cur().kind == tkOp && isCmpOp(p.cur().text) {
		op := p.advance().text
		right, err := p.parsePrimary()
		if err != nil {
			return nil, err
		}
		// Type-check: both sides must be the same comparable kind.
		lt, rt := left.typ(), right.typ()
		if lt == typeBool || rt == typeBool {
			return nil, fmt.Errorf("cannot compare boolean values with %s", op)
		}
		if lt != rt {
			return nil, fmt.Errorf("cannot compare %s with %s using %s", lt, rt, op)
		}
		return cmpNode{op: op, l: left, r: right, numeric: lt == typeInt}, nil
	}
	return left, nil
}

func (p *parser) parsePrimary() (node, error) {
	t := p.cur()
	switch t.kind {
	case tkLParen:
		p.advance()
		inner, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if p.cur().kind != tkRParen {
			return nil, fmt.Errorf("missing closing ) at position %d", p.cur().pos)
		}
		p.advance()
		return inner, nil
	case tkInt:
		p.advance()
		return intLit{n: t.ival}, nil
	case tkStr:
		p.advance()
		return strLit{s: t.text}, nil
	case tkBool:
		p.advance()
		return boolLit{b: t.bval}, nil
	case tkIdent:
		p.advance()
		if !knownVars[t.text] {
			return nil, fmt.Errorf("unknown variable %q at position %d (allowed: w, t, tokens, group, model)", t.text, t.pos)
		}
		vt := typeStr
		if numericVars[t.text] {
			vt = typeInt
		}
		return varRef{name: t.text, vt: vt}, nil
	default:
		return nil, fmt.Errorf("unexpected %q at position %d", t.text, t.pos)
	}
}

func isCmpOp(s string) bool {
	switch s {
	case "==", "!=", "<", "<=", ">", ">=":
		return true
	}
	return false
}
