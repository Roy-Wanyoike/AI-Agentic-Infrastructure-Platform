package workflows

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ---------------------------------------------------------------------------
// Safe expression engine for workflow condition nodes (issue #109).
//
// The engine is a small, dependency-free expression language evaluated by a
// hand-written tokenizer + recursive-descent parser + tree-walking evaluator.
// It is intentionally NOT a general scripting language:
//
//   - the only root references are $.nodes.<id>.output.<path> (previous node
//     outputs), $.input (the workflow input string), $.item / $.loop.index
//     (current loop iteration), $.context.<key>... (executor-provided values
//     such as polls/workflow ids) — everything else is a typed
//     expr_unknown_identifier error, so expressions can never reach arbitrary
//     Go state;
//   - there are no function calls, no imports, no reflection and no I/O;
//   - abuse is bounded by hard caps (MaxExpressionLength bytes,
//     MaxExpressionDepth parse depth).
//
// Grammar (precedence low -> high):
//
//      or      := and ("or" and)*
//      and     := cmp ("and" cmp)*
//      cmp     := add (("=="|"!="|"<"|"<="|">"|">=") add)?
//      add     := mul (("+"|"-") mul)*
//      mul     := unary (("*"|"/") unary)*
//      unary   := ("not"|"-"|"!")* primary
//      primary := number | string | true | false | null | "(" or ")" | path
//      path    := "$" ("." ident | "[" number "]")*
// ---------------------------------------------------------------------------

// Hard resource caps. Expressions beyond these limits are rejected with typed
// errors before parsing so a malicious DSL cannot inflate the parser/evaluator.
const (
	// MaxExpressionLength bounds the serialized expression size (4KB).
	MaxExpressionLength = 4096
	// MaxExpressionDepth bounds the parse/AST depth (paren nesting + unary
	// chains); it keeps recursion inside the parser and evaluator constant
	// per expression no matter how it is written.
	MaxExpressionDepth = 64
)

// Expression error codes surfaced through ExprError.Code (and, at workflow
// save time, through ValidationError with code invalid_expression).
const (
	ExprErrSyntax          = "expr_syntax"
	ExprErrUnknownIdent    = "expr_unknown_identifier"
	ExprErrUnknownFunction = "expr_unknown_function"
	ExprErrTypeMismatch    = "expr_type_mismatch"
	ExprErrDivisionByZero  = "expr_division_by_zero"
	ExprErrDepthExceeded   = "expr_depth_exceeded"
	ExprErrLengthExceeded  = "expr_length_exceeded"
	ExprErrPathNotFound    = "expr_path_not_found"
)

// ExprError is the typed error returned for every expression failure. Code is
// a stable machine identifier (see the ExprErr* constants); Pos is the byte
// offset into the source expression where the failure was detected (-1 when
// not applicable, e.g. depth/length caps or path resolution).
type ExprError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Pos     int    `json:"pos"`
}

// Error implements the error interface with a machine-parsable prefix.
func (e *ExprError) Error() string {
	if e.Pos >= 0 {
		return fmt.Sprintf("%s: %s (at offset %d)", e.Code, e.Message, e.Pos)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func exprErrf(code string, pos int, format string, args ...any) *ExprError {
	return &ExprError{Code: code, Message: fmt.Sprintf(format, args...), Pos: pos}
}

// asExprError unwraps err (following wrapped errors) into the first *ExprError
// it finds. Shared by validation, the executor's error-code stamping and tests.
func asExprError(err error, target **ExprError) bool {
	for err != nil {
		if e, ok := err.(*ExprError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// EvalEnv carries the values an expression may reference. Outputs maps node
// id -> raw JSON output document (populated by the executor as nodes finish);
// Input is the raw workflow input string; Item/LoopIndex are set while a loop
// body executes; Context holds executor-provided values reachable via
// $.context.<key> (e.g. workflow ids, wait_until poll counters).
type EvalEnv struct {
	Outputs   map[string]json.RawMessage
	Input     string
	Item      any
	LoopIndex int // -1 outside loop bodies
	Context   map[string]any
}

// NewEvalEnv returns an EvalEnv with the loop index defaulted to "outside
// loop" and the maps allocated, so callers can mutate before evaluation.
func NewEvalEnv() EvalEnv {
	return EvalEnv{Outputs: map[string]json.RawMessage{}, LoopIndex: -1, Context: map[string]any{}}
}

// ---------------------------------------------------------------------------
// Tokenizer
// ---------------------------------------------------------------------------

type tokenKind int

const (
	tkEOF tokenKind = iota
	tkNumber
	tkString
	tkIdent
	tkDollar
	tkDot
	tkLparen
	tkRparen
	tkLbrack
	tkRbrack
	tkOp // == != <= >= < > + - * /
)

type token struct {
	kind tokenKind
	text string
	pos  int
}

// tokenize converts the expression source into a token stream. It rejects
// unknown bytes with expr_syntax and over-long sources with
// expr_length_exceeded before any allocation-heavy work happens.
func tokenize(src string) ([]token, error) {
	if len(src) > MaxExpressionLength {
		return nil, exprErrf(ExprErrLengthExceeded, -1, "expression is %d bytes, exceeding the %d byte cap", len(src), MaxExpressionLength)
	}
	toks := make([]token, 0, 16)
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '$':
			toks = append(toks, token{tkDollar, "$", i})
			i++
		case c == '.':
			toks = append(toks, token{tkDot, ".", i})
			i++
		case c == '(':
			toks = append(toks, token{tkLparen, "(", i})
			i++
		case c == ')':
			toks = append(toks, token{tkRparen, ")", i})
			i++
		case c == '[':
			toks = append(toks, token{tkLbrack, "[", i})
			i++
		case c == ']':
			toks = append(toks, token{tkRbrack, "]", i})
			i++
		case c == '\'' || c == '"':
			start := i
			quote := c
			i++
			var sb strings.Builder
			closed := false
			for i < len(src) {
				ch := src[i]
				if ch == '\\' && i+1 < len(src) {
					esc := src[i+1]
					switch esc {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					case '\\', '\'', '"', '/':
						sb.WriteByte(esc)
					default:
						return nil, exprErrf(ExprErrSyntax, i, "unsupported escape sequence \\%c", esc)
					}
					i += 2
					continue
				}
				if ch == quote {
					closed = true
					i++
					break
				}
				sb.WriteByte(ch)
				i++
			}
			if !closed {
				return nil, exprErrf(ExprErrSyntax, start, "unterminated string literal")
			}
			toks = append(toks, token{tkString, sb.String(), start})
		case c >= '0' && c <= '9':
			start := i
			for i < len(src) && src[i] >= '0' && src[i] <= '9' {
				i++
			}
			if i < len(src) && src[i] == '.' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9' {
				i++
				for i < len(src) && src[i] >= '0' && src[i] <= '9' {
					i++
				}
			}
			toks = append(toks, token{tkNumber, src[start:i], start})
		case unicode.IsLetter(rune(c)) || c == '_':
			start := i
			for i < len(src) && (unicode.IsLetter(rune(src[i])) || unicode.IsDigit(rune(src[i])) || src[i] == '_') {
				i++
			}
			toks = append(toks, token{tkIdent, src[start:i], start})
		case i+1 < len(src) && (src[i:i+2] == "==" || src[i:i+2] == "!=" || src[i:i+2] == "<=" || src[i:i+2] == ">="):
			toks = append(toks, token{tkOp, src[i : i+2], i})
			i += 2
		case c == '<' || c == '>' || c == '+' || c == '-' || c == '*' || c == '/' || c == '!':
			// A bare '!' is accepted as the alias of "not"; '=' alone is a syntax error.
			if c == '=' {
				return nil, exprErrf(ExprErrSyntax, i, "unexpected '=' (use '==' for equality)")
			}
			toks = append(toks, token{tkOp, string(c), i})
			i++
		default:
			return nil, exprErrf(ExprErrSyntax, i, "unexpected character %q", string(rune(c)))
		}
	}
	toks = append(toks, token{tkEOF, "", len(src)})
	return toks, nil
}

// ---------------------------------------------------------------------------
// AST
// ---------------------------------------------------------------------------

type exprNode interface{}

type literalNode struct {
	value any // float64 | string | bool | nil
}

type unaryNode struct {
	op   string // "not" | "-" | "!"
	expr exprNode
}

type binaryNode struct {
	op          string
	left, right exprNode
}

type pathSegment struct {
	key   string // object key access (when isIndex is false)
	index int    // array index access (when isIndex is true)
	isIdx bool
}

type pathNode struct {
	segments []pathSegment
}

// ---------------------------------------------------------------------------
// Parser (recursive descent with depth cap)
// ---------------------------------------------------------------------------

// parser descends the grammar with a depth guard on the two recursion
// amplifiers: parenthesized groups (parsePrimary) and unary operator chains
// (parseUnary). The precedence-level functions recurse only through those
// two, so the cap bounds the total AST depth of an expression.
type parser struct {
	toks  []token
	pos   int
	depth int
}

func (p *parser) cur() token  { return p.toks[p.pos] }
func (p *parser) next() token { t := p.toks[p.pos]; p.pos++; return t }
func (p *parser) atEOF() bool { return p.cur().kind == tkEOF }
func (p *parser) enter() error {
	p.depth++
	if p.depth > MaxExpressionDepth {
		return exprErrf(ExprErrDepthExceeded, -1, "expression exceeds the maximum depth of %d", MaxExpressionDepth)
	}
	return nil
}
func (p *parser) leave() { p.depth-- }

func (p *parser) expectOp(text string) error {
	t := p.cur()
	if t.kind != tkOp || t.text != text {
		return exprErrf(ExprErrSyntax, t.pos, "expected %q, got %q", text, tokenText(t))
	}
	p.pos++
	return nil
}

func tokenText(t token) string {
	if t.kind == tkEOF {
		return "end of expression"
	}
	return t.text
}

// parseExpression parses a full expression and enforces a single trailing EOF.
// It also statically rejects unknown path roots so invalid references surface
// at parse time (and therefore at workflow save time) rather than mid-run.
func parseExpression(src string) (exprNode, error) {
	toks, err := tokenize(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	node, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if !p.atEOF() {
		t := p.cur()
		return nil, exprErrf(ExprErrSyntax, t.pos, "unexpected trailing %q", tokenText(t))
	}
	if err := validateStaticRoots(node); err != nil {
		return nil, err
	}
	return node, nil
}

// validateStaticRoots walks the AST and rejects path roots outside the
// documented reference set (nodes, input, item, loop, context). Literals are
// skipped; this check needs no evaluation environment, so it is safe to run
// at workflow save time when no node outputs exist yet.
func validateStaticRoots(node exprNode) error {
	switch n := node.(type) {
	case *pathNode:
		if len(n.segments) == 0 {
			return nil // parsePath already rejects empty paths
		}
		switch n.segments[0].key {
		case "nodes", "input", "item", "loop", "context":
			return nil
		default:
			return exprErrf(ExprErrUnknownIdent, -1, "unknown root %q (expected nodes, input, item, loop or context)", n.segments[0].key)
		}
	case *unaryNode:
		return validateStaticRoots(n.expr)
	case *binaryNode:
		if err := validateStaticRoots(n.left); err != nil {
			return err
		}
		return validateStaticRoots(n.right)
	}
	return nil
}

// ValidateExpressionSyntax checks that expr parses (tokenizer + recursive
// descent + static root check) WITHOUT evaluating any path references: at
// workflow save time no node outputs exist, so $.nodes.<id>.output.<path> /
// $.context.<key> references resolve at run time only. Syntax errors, bare
// identifiers, function calls and unknown roots still surface here as typed
// *ExprError values (see the ExprErr* codes).
func ValidateExpressionSyntax(expr string) error {
	if strings.TrimSpace(expr) == "" {
		return exprErrf(ExprErrSyntax, -1, "expression is empty")
	}
	_, err := parseExpression(expr)
	return err
}

func (p *parser) parseOr() (exprNode, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.isKeyword("or") {
		p.pos++
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: "or", left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (exprNode, error) {
	left, err := p.parseCmp()
	if err != nil {
		return nil, err
	}
	for p.isKeyword("and") {
		p.pos++
		right, err := p.parseCmp()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: "and", left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseCmp() (exprNode, error) {
	left, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	if t := p.cur(); t.kind == tkOp && (t.text == "==" || t.text == "!=" || t.text == "<" || t.text == "<=" || t.text == ">" || t.text == ">=") {
		p.pos++
		right, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		return &binaryNode{op: t.text, left: left, right: right}, nil
	}
	return left, nil
}

func (p *parser) parseAdd() (exprNode, error) {
	left, err := p.parseMul()
	if err != nil {
		return nil, err
	}
	for t := p.cur(); t.kind == tkOp && (t.text == "+" || t.text == "-"); t = p.cur() {
		p.pos++
		right, err := p.parseMul()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: t.text, left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseMul() (exprNode, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for t := p.cur(); t.kind == tkOp && (t.text == "*" || t.text == "/"); t = p.cur() {
		p.pos++
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: t.text, left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseUnary() (exprNode, error) {
	t := p.cur()
	isBang := t.kind == tkOp && (t.text == "-" || t.text == "!")
	if isBang || p.isKeyword("not") {
		// Depth is charged only when a unary operator is actually consumed;
		// the fall-through to parsePrimary is free so that paren nesting
		// counts exactly one level per '('.
		if err := p.enter(); err != nil {
			return nil, err
		}
		defer p.leave()
		op := "not"
		if isBang {
			op = t.text
		}
		p.pos++
		inner, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &unaryNode{op: op, expr: inner}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (exprNode, error) {
	t := p.cur()
	switch t.kind {
	case tkNumber:
		p.pos++
		v, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, exprErrf(ExprErrSyntax, t.pos, "invalid number %q", t.text)
		}
		return &literalNode{value: v}, nil
	case tkString:
		p.pos++
		return &literalNode{value: t.text}, nil
	case tkLparen:
		// Depth is charged per parenthesized group so MaxExpressionDepth
		// counts paren nesting (plus unary chains, see parseUnary).
		if err := p.enter(); err != nil {
			return nil, err
		}
		defer p.leave()
		p.pos++
		inner, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.cur().kind != tkRparen {
			return nil, exprErrf(ExprErrSyntax, p.cur().pos, "expected ')', got %q", tokenText(p.cur()))
		}
		p.pos++
		return inner, nil
	case tkDollar:
		return p.parsePath()
	case tkIdent:
		// Function-call syntax is explicitly rejected with its own code so
		// users get a clear signal instead of a generic syntax error.
		if p.pos+1 < len(p.toks) && p.toks[p.pos+1].kind == tkLparen {
			return nil, exprErrf(ExprErrUnknownFunction, t.pos, "function calls are not supported (unknown function %q)", t.text)
		}
		switch strings.ToLower(t.text) {
		case "true":
			p.pos++
			return &literalNode{value: true}, nil
		case "false":
			p.pos++
			return &literalNode{value: false}, nil
		case "null", "nil":
			p.pos++
			return &literalNode{value: nil}, nil
		}
		return nil, exprErrf(ExprErrUnknownIdent, t.pos, "unknown identifier %q (only $.nodes, $.input, $.item, $.loop, $.context roots and literals are supported)", t.text)
	default:
		return nil, exprErrf(ExprErrSyntax, t.pos, "unexpected token %q", tokenText(t))
	}
}

func (p *parser) isKeyword(word string) bool {
	t := p.cur()
	return t.kind == tkIdent && strings.EqualFold(t.text, word)
}

// parsePath parses `$` followed by .key / [index] segments.
func (p *parser) parsePath() (exprNode, error) {
	p.pos++ // consume '$'
	node := &pathNode{}
	seen := false
	for {
		t := p.cur()
		switch t.kind {
		case tkDot:
			p.pos++
			seg := p.cur()
			if seg.kind != tkIdent {
				return nil, exprErrf(ExprErrSyntax, seg.pos, "expected identifier after '.', got %q", tokenText(seg))
			}
			node.segments = append(node.segments, pathSegment{key: seg.text})
			seen = true
			p.pos++
		case tkLbrack:
			p.pos++
			idx := p.cur()
			if idx.kind != tkNumber {
				return nil, exprErrf(ExprErrSyntax, idx.pos, "expected numeric index inside '[...]', got %q", tokenText(idx))
			}
			n, err := strconv.ParseFloat(idx.text, 64)
			if err != nil || n != float64(int(n)) || n < 0 {
				return nil, exprErrf(ExprErrSyntax, idx.pos, "array index must be a non-negative integer, got %q", idx.text)
			}
			node.segments = append(node.segments, pathSegment{index: int(n), isIdx: true})
			seen = true
			p.pos++
			if p.cur().kind != tkRbrack {
				return nil, exprErrf(ExprErrSyntax, p.cur().pos, "expected ']' after array index")
			}
			p.pos++
		default:
			if !seen {
				return nil, exprErrf(ExprErrSyntax, t.pos, "path must start with a segment ($.nodes, $.input, $.item, $.loop, $.context)")
			}
			return node, nil
		}
	}
}

// ---------------------------------------------------------------------------
// Evaluator
// ---------------------------------------------------------------------------

// EvaluateExpression parses and evaluates one expression against env. All
// failures are *ExprError (typed, with stable codes). Numbers are float64
// (JSON semantics); `+` concatenates strings when both operands are strings.
func EvaluateExpression(expr string, env EvalEnv) (any, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, exprErrf(ExprErrSyntax, -1, "expression is empty")
	}
	ast, err := parseExpression(expr)
	if err != nil {
		return nil, err
	}
	return evalNode(ast, env)
}

// EvaluateCondition evaluates expr and coerces the result to a boolean using
// documented truthiness rules (bool -> itself, nil/0/""/empty collections ->
// false, everything else -> true). Condition, switch, loop and wait_until
// nodes route on this coercion.
func EvaluateCondition(expr string, env EvalEnv) (bool, error) {
	v, err := EvaluateExpression(expr, env)
	if err != nil {
		return false, err
	}
	return Truthy(v), nil
}

// Truthy reports the documented truthiness of a value: false for nil, zero
// numbers, empty strings and empty arrays/maps; true otherwise.
func Truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	default:
		return true
	}
}

func evalNode(node exprNode, env EvalEnv) (any, error) {
	switch n := node.(type) {
	case *literalNode:
		return n.value, nil
	case *pathNode:
		return resolvePath(n.segments, env)
	case *unaryNode:
		return evalUnary(n, env)
	case *binaryNode:
		return evalBinary(n, env)
	default:
		return nil, exprErrf(ExprErrSyntax, -1, "unsupported AST node %T", node)
	}
}

func evalUnary(n *unaryNode, env EvalEnv) (any, error) {
	v, err := evalNode(n.expr, env)
	if err != nil {
		return nil, err
	}
	switch n.op {
	case "not", "!":
		return !Truthy(v), nil
	case "-":
		num, ok := v.(float64)
		if !ok {
			return nil, exprErrf(ExprErrTypeMismatch, -1, "unary '-' requires a number, got %s", typeName(v))
		}
		return -num, nil
	default:
		return nil, exprErrf(ExprErrSyntax, -1, "unknown unary operator %q", n.op)
	}
}

func evalBinary(n *binaryNode, env EvalEnv) (any, error) {
	switch n.op {
	case "and", "or":
		left, err := evalNode(n.left, env)
		if err != nil {
			return nil, err
		}
		// Short-circuit on the left operand's truthiness.
		if n.op == "and" && !Truthy(left) {
			return left, nil
		}
		if n.op == "or" && Truthy(left) {
			return left, nil
		}
		return evalNode(n.right, env)
	}

	left, err := evalNode(n.left, env)
	if err != nil {
		return nil, err
	}
	right, err := evalNode(n.right, env)
	if err != nil {
		return nil, err
	}

	switch n.op {
	case "==":
		return valuesEqual(left, right), nil
	case "!=":
		return !valuesEqual(left, right), nil
	case "<", "<=", ">", ">=":
		return compareValues(n.op, left, right)
	case "+":
		if ls, ok := left.(string); ok {
			if rs, ok := right.(string); ok {
				return ls + rs, nil
			}
			return nil, exprErrf(ExprErrTypeMismatch, -1, "'+' on strings requires a string operand, got %s", typeName(right))
		}
		return arith(n.op, left, right)
	case "-", "*", "/":
		return arith(n.op, left, right)
	default:
		return nil, exprErrf(ExprErrSyntax, -1, "unknown binary operator %q", n.op)
	}
}

func arith(op string, left, right any) (any, error) {
	l, lok := left.(float64)
	r, rok := right.(float64)
	if !lok || !rok {
		return nil, exprErrf(ExprErrTypeMismatch, -1, "arithmetic '%s' requires numeric operands, got %s and %s", op, typeName(left), typeName(right))
	}
	switch op {
	case "+":
		return l + r, nil
	case "-":
		return l - r, nil
	case "*":
		return l * r, nil
	case "/":
		if r == 0 {
			return nil, exprErrf(ExprErrDivisionByZero, -1, "division by zero")
		}
		return l / r, nil
	}
	return nil, exprErrf(ExprErrSyntax, -1, "unknown arithmetic operator %q", op)
}

// valuesEqual reports deep equality; values of different types are simply not
// equal (no coercion) while numbers compare numerically.
func valuesEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	af, aok := a.(float64)
	bf, bok := b.(float64)
	if aok && bok {
		return af == bf
	}
	if aok != bok {
		return false
	}
	switch av := a.(type) {
	case string:
		bs, ok := b.(string)
		return ok && av == bs
	case bool:
		bb, ok := b.(bool)
		return ok && av == bb
	default:
		return jsonEqual(a, b)
	}
}

// jsonEqual compares composite values by their canonical JSON encoding.
func jsonEqual(a, b any) bool {
	ab, aerr := json.Marshal(a)
	bb, berr := json.Marshal(b)
	if aerr != nil || berr != nil {
		return false
	}
	return string(ab) == string(bb)
}

func compareValues(op string, left, right any) (any, error) {
	switch l := left.(type) {
	case float64:
		r, ok := right.(float64)
		if !ok {
			return nil, exprErrf(ExprErrTypeMismatch, -1, "cannot order %s and %s", typeName(left), typeName(right))
		}
		return compareOrdered(op, l-r), nil
	case string:
		r, ok := right.(string)
		if !ok {
			return nil, exprErrf(ExprErrTypeMismatch, -1, "cannot order %s and %s", typeName(left), typeName(right))
		}
		return compareOrdered(op, float64(strings.Compare(l, r))), nil
	default:
		return nil, exprErrf(ExprErrTypeMismatch, -1, "ordering operators require numbers or strings, got %s", typeName(left))
	}
}

func compareOrdered(op string, diff float64) bool {
	switch op {
	case "<":
		return diff < 0
	case "<=":
		return diff <= 0
	case ">":
		return diff > 0
	case ">=":
		return diff >= 0
	}
	return false
}

func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case json.RawMessage:
		return "json"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// ---------------------------------------------------------------------------
// Path resolution ($.nodes.<id>.output.<path>, $.input, $.item, $.loop,
// $.context.<key>)
// ---------------------------------------------------------------------------

// resolvePath resolves a parsed path against the evaluation environment.
// Unknown roots/node ids raise expr_unknown_identifier; walking into a missing
// key or out-of-range index raises expr_path_not_found.
func resolvePath(segments []pathSegment, env EvalEnv) (any, error) {
	if len(segments) == 0 {
		return nil, exprErrf(ExprErrSyntax, -1, "path must reference a root (nodes, input, item, loop, context)")
	}
	root := segments[0].key
	switch root {
	case "nodes":
		return resolveNodesPath(segments[1:], env)
	case "input":
		if len(segments) > 1 {
			return nil, exprErrf(ExprErrPathNotFound, -1, "$.input is a scalar; cannot descend into %q", segments[1].key)
		}
		return env.Input, nil
	case "item":
		if env.Item == nil {
			return nil, nil // outside loops $.item is null
		}
		return walkValue(env.Item, segments[1:])
	case "loop":
		return walkValue(map[string]any{"index": float64(env.LoopIndex)}, segments[1:])
	case "context":
		if len(segments) < 2 {
			return nil, exprErrf(ExprErrSyntax, -1, "$.context requires a key ($.context.<key>...)")
		}
		key := segments[1].key
		v, ok := env.Context[key]
		if !ok {
			return nil, exprErrf(ExprErrUnknownIdent, -1, "unknown context key %q", key)
		}
		return walkValue(v, segments[2:])
	default:
		return nil, exprErrf(ExprErrUnknownIdent, -1, "unknown root %q (expected nodes, input, item, loop or context)", root)
	}
}

// resolveNodesPath handles everything after `$.nodes`. The canonical node
// reference is $.nodes.<id>.output.<path>; the literal segment `output`
// directly after the node id is the marker for "the node's output document"
// and is consumed, so $.nodes.x.output and $.nodes.x resolve identically.
func resolveNodesPath(segments []pathSegment, env EvalEnv) (any, error) {
	if len(segments) == 0 || segments[0].isIdx {
		return nil, exprErrf(ExprErrSyntax, -1, "$.nodes requires a node id ($.nodes.<id>.output...)")
	}
	nodeID := segments[0].key
	raw, ok := env.Outputs[nodeID]
	if !ok {
		return nil, exprErrf(ExprErrUnknownIdent, -1, "unknown node id %q (no recorded output)", nodeID)
	}
	rest := segments[1:]
	if len(rest) > 0 && !rest[0].isIdx && rest[0].key == "output" {
		rest = rest[1:] // canonical marker segment
	}
	if len(rest) == 0 {
		// Whole output document, decoded.
		return decodeJSONDoc(raw)
	}
	doc, err := decodeJSONDoc(raw)
	if err != nil {
		return nil, err
	}
	return walkValue(doc, rest)
}

// decodeJSONDoc decodes one node output document into generic Go values.
func decodeJSONDoc(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, exprErrf(ExprErrSyntax, -1, "node output is not valid JSON: %v", err)
	}
	return doc, nil
}

// walkValue descends into decoded JSON values following the remaining path
// segments (object keys / array indexes).
func walkValue(v any, segments []pathSegment) (any, error) {
	for _, seg := range segments {
		switch cur := v.(type) {
		case map[string]any:
			if seg.isIdx {
				return nil, exprErrf(ExprErrPathNotFound, -1, "cannot index an object with [%d]", seg.index)
			}
			next, ok := cur[seg.key]
			if !ok {
				return nil, exprErrf(ExprErrPathNotFound, -1, "path segment %q not found", seg.key)
			}
			v = next
		case []any:
			if !seg.isIdx {
				return nil, exprErrf(ExprErrPathNotFound, -1, "cannot access key %q of an array (use [index])", seg.key)
			}
			if seg.index < 0 || seg.index >= len(cur) {
				return nil, exprErrf(ExprErrPathNotFound, -1, "array index [%d] out of range (length %d)", seg.index, len(cur))
			}
			v = cur[seg.index]
		default:
			return nil, exprErrf(ExprErrPathNotFound, -1, "cannot descend into %s value", typeName(v))
		}
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// Condition-node configuration parsing (shared by validation + execution)
// ---------------------------------------------------------------------------

// BranchTargets is the config.branches document of an if/condition node: the
// node ids routed to when the expression evaluates truthy / falsy. Either
// side may be empty (that side of the branch simply does not continue).
type BranchTargets struct {
	True  string
	False string
}

// BranchTargetsFromConfig reads config.branches {true, false} from a
// condition node's config. Missing/empty branches yield empty targets.
func BranchTargetsFromConfig(config map[string]any) BranchTargets {
	raw, ok := config["branches"].(map[string]any)
	if !ok {
		return BranchTargets{}
	}
	return BranchTargets{True: configString(raw, "true"), False: configString(raw, "false")}
}

// DefaultLoopMaxIterations is the loop cap applied when config.max_iterations
// is absent (issue #109: default 100).
const DefaultLoopMaxIterations = 100

// MaxLoopIterationsHardCap bounds user-provided max_iterations regardless of
// DSL content, so even validated workflows cannot spin unbounded.
const MaxLoopIterationsHardCap = 10000

// LoopConfig is the parsed config of a loop node: the expression yielding the
// array to iterate, the iteration cap and the body node ids (executed once
// per item, in body-declaration order subject to body-internal edges).
type LoopConfig struct {
	Expression    string
	MaxIterations int
	Body          []string
}

// LoopConfigFromNode parses a loop node's config with defaults applied.
func LoopConfigFromNode(n Node) LoopConfig {
	cfg := LoopConfig{Expression: configString(n.Config, "expression"), MaxIterations: DefaultLoopMaxIterations}
	if v := configFloat(n.Config, "max_iterations"); v > 0 {
		cfg.MaxIterations = int(v)
	}
	if cfg.MaxIterations > MaxLoopIterationsHardCap {
		cfg.MaxIterations = MaxLoopIterationsHardCap
	}
	if raw, ok := n.Config["body"].([]any); ok {
		for _, item := range raw {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				cfg.Body = append(cfg.Body, strings.TrimSpace(s))
			}
		}
	}
	return cfg
}

// DefaultRetryAttempts / DefaultRetryBackoff are applied when a retry node's
// config omits them.
const (
	DefaultRetryAttempts = 3
	// DefaultRetryBaseDelay is the first backoff delay.
	DefaultRetryBaseDelay = 100 * time.Millisecond
	// DefaultRetryMultiplier grows the delay geometrically.
	DefaultRetryMultiplier = 2.0
	// DefaultRetryMaxDelay bounds every individual backoff sleep.
	DefaultRetryMaxDelay = 30 * time.Second
	// MaxRetryAttemptsHardCap bounds user-provided attempts.
	MaxRetryAttemptsHardCap = 100
)

// RetryPolicy is the backoff policy of a retry node: attempts total (not
// retries), base delay, geometric multiplier and per-sleep maximum.
type RetryPolicy struct {
	Attempts   int           `json:"attempts"`
	BaseDelay  time.Duration `json:"base_delay_ms"`
	Multiplier float64       `json:"multiplier"`
	MaxDelay   time.Duration `json:"max_delay_ms"`
}

// DefaultRetryPolicy returns the documented defaults (3 attempts, 100ms base,
// x2 multiplier, 30s max).
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{Attempts: DefaultRetryAttempts, BaseDelay: DefaultRetryBaseDelay, Multiplier: DefaultRetryMultiplier, MaxDelay: DefaultRetryMaxDelay}
}

// DelayFor returns the backoff delay before the given attempt is retried
// (attempt is 1-based: the delay after the first failure is DelayFor(1)).
func (p RetryPolicy) DelayFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if p.Multiplier < 1 {
		return p.MaxDelay
	}
	d := p.BaseDelay
	for i := 1; i < attempt; i++ {
		d = time.Duration(float64(d) * p.Multiplier)
		if d >= p.MaxDelay {
			return p.MaxDelay
		}
	}
	if d > p.MaxDelay {
		return p.MaxDelay
	}
	if d < 0 {
		return 0
	}
	return d
}

// RetryConfigFromNode parses a retry node's config: target node id, attempts
// and the nested backoff policy.
func RetryConfigFromNode(n Node) (target string, policy RetryPolicy) {
	target = configString(n.Config, "target")
	policy = DefaultRetryPolicy()
	if v := configFloat(n.Config, "attempts"); v >= 1 {
		policy.Attempts = int(v)
	}
	if policy.Attempts > MaxRetryAttemptsHardCap {
		policy.Attempts = MaxRetryAttemptsHardCap
	}
	if raw, ok := n.Config["backoff"].(map[string]any); ok {
		if v := configFloat(raw, "base_delay_ms"); v > 0 {
			policy.BaseDelay = time.Duration(v) * time.Millisecond
		}
		if v := configFloat(raw, "multiplier"); v >= 1 {
			policy.Multiplier = v
		}
		if v := configFloat(raw, "max_delay_ms"); v > 0 {
			policy.MaxDelay = time.Duration(v) * time.Millisecond
		}
	}
	return target, policy
}

// DefaultWaitTimeout / DefaultWaitPollInterval are applied when a wait_until
// node's config omits them.
const (
	DefaultWaitTimeout      = 60 * time.Second
	DefaultWaitPollInterval = 1 * time.Second
	// MinWaitPollInterval keeps the poll loop from becoming a busy spin on
	// the real clock.
	MinWaitPollInterval = 10 * time.Millisecond
	// ErrorCodeWaitUntilTimeout is stamped on wait_until node runs that hit
	// their deadline before the expression turned truthy.
	ErrorCodeWaitUntilTimeout = "WAIT_UNTIL_TIMEOUT"
)

// WaitUntilConfig is the parsed config of a wait_until node: the expression
// polled for truthiness, the overall timeout and the poll interval.
type WaitUntilConfig struct {
	Expression   string
	Timeout      time.Duration
	PollInterval time.Duration
}

// WaitUntilConfigFromNode parses a wait_until node's config with defaults.
func WaitUntilConfigFromNode(n Node) WaitUntilConfig {
	cfg := WaitUntilConfig{
		Expression:   configString(n.Config, "expression"),
		Timeout:      DefaultWaitTimeout,
		PollInterval: DefaultWaitPollInterval,
	}
	if v := configFloat(n.Config, "timeout_seconds"); v > 0 {
		cfg.Timeout = time.Duration(v * float64(time.Second))
	}
	if v := configFloat(n.Config, "poll_interval_ms"); v > 0 {
		cfg.PollInterval = time.Duration(v) * time.Millisecond
	}
	if cfg.PollInterval < MinWaitPollInterval {
		cfg.PollInterval = MinWaitPollInterval
	}
	return cfg
}

// switchValueMatches reports whether an evaluated switch operand matches a
// case key, using the canonical SwitchMatchValue serialization on both sides
// so `3 == "3"`, `true == "true"` and `null == "null"` all match.
func switchValueMatches(v any, caseKey string) bool {
	return SwitchMatchValue(v) == caseKey
}

// SwitchMatchValue serializes an evaluated switch operand into the canonical
// string used to match case keys: strings verbatim, numbers via their
// shortest float representation, booleans as true/false, null as "null".
func SwitchMatchValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprintf("%v", x)
		}
		return string(b)
	}
}
