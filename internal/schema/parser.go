package schema

import (
	"fmt"

	internalNamespace "github.com/ory/keto/internal/namespace"
	"github.com/ory/keto/internal/namespace/ast"
)

type (
	namespace = internalNamespace.Namespace

	parser struct {
		lexer      *lexer        // lexer to get tokens from
		namespaces []namespace   // list of parsed namespaces
		namespace  namespace     // current namespace
		errors     []*ParseError // errors encountered during parsing
		fatal      bool          // parser encountered a fatal error
		lookahead  *item         // lookahead token
		checks     []typeCheck   // checks to perform on the namespace
	}
)

func Parse(input string) ([]namespace, []*ParseError) {
	p := &parser{
		lexer: Lex("input", input),
	}
	return p.parse()
}

func (p *parser) next() (item item) {
	if p.lookahead != nil {
		item = *p.lookahead
		p.lookahead = nil
	} else {
		// Get next non-comment token.
		for item = p.lexer.nextItem(); item.Typ == itemComment; item = p.lexer.nextItem() {
		}
	}
	return
}

func (p *parser) peek() item {
	if p.lookahead == nil {
		i := p.lexer.nextItem()
		p.lookahead = &i
		return i
	}
	return *p.lookahead
}

func (p *parser) parse() ([]namespace, []*ParseError) {
loop:
	for !p.fatal {
		switch item := p.next(); item.Typ {
		case itemEOF:
			break loop
		case itemError:
			p.addFatal(item, "fatal: %s", item.Val)
		case itemKeywordClass:
			p.parseClass()
		}
	}

	if len(p.errors) == 0 {
		p.typeCheck()
	}

	return p.namespaces, p.errors
}

func (p *parser) addFatal(item item, format string, a ...interface{}) {
	p.addErr(item, format, a...)
	p.fatal = true
}
func (p *parser) addErr(item item, format string, a ...interface{}) {
	err := &ParseError{
		msg:  fmt.Sprintf(format, a...),
		item: item,
		p:    p,
	}
	p.errors = append(p.errors, err)
}

type matcher func(p *parser) (matched bool)

// optional optionally matches the first argument of tokens in the input. If
// matched, the tokens are consumed. If the first token matched, all other
// tokens must match as well.
func optional(tokens ...string) matcher {
	return func(p *parser) bool {
		if len(tokens) == 0 {
			return true
		}
		first := tokens[0]
		if p.peek().Val == first {
			p.next()
			for _, token := range tokens[1:] {
				i := p.next()
				if i.Val != token {
					p.addFatal(i, "expected %q, got %q", token, i.Val)
					return false
				}
			}
		}
		return true
	}
}

// match for the next tokens in the input.
//
// A token is matched depending on the type:
// For string arguments, the input token must match the given string exactly.
// For *string arguments, the input token must be an identifier, and the value
// of the identifier will be written to the *string.
// For *item arguments, the input token will be written to the pointer.
func (p *parser) match(tokens ...interface{}) (matched bool) {
	if p.fatal {
		return false
	}

	for _, token := range tokens {
		switch token := token.(type) {
		case string:
			i := p.next()
			if i.Val != token {
				p.addFatal(i, "expected %q, got %q", token, i.Val)
				return false
			}
		case *string:
			i := p.next()
			if i.Typ != itemIdentifier && i.Typ != itemStringLiteral {
				p.addFatal(i, "expected identifier, got %s", i.Typ)
				return false
			}
			*token = i.Val
		case *item:
			*token = p.next()
		case matcher:
			if !token(p) {
				return false
			}
		}
	}
	return true
}

type itemPredicate func(item) bool

func is(typ itemType) itemPredicate {
	return func(item item) bool {
		return item.Typ == typ
	}
}

// matchIf matches the tokens iff. the predicate is true.
func (p *parser) matchIf(predicate itemPredicate, tokens ...interface{}) (matched bool) {
	if p.fatal {
		return false
	}
	if !predicate(p.peek()) {
		return false
	}
	return p.match(tokens...)
}

// parseClass parses a class. The "class" token was already consumed.
func (p *parser) parseClass() {
	var name string
	p.match(&name, "implements", "Namespace", "{")
	p.namespace = namespace{Name: name}

	for !p.fatal {
		switch item := p.next(); {
		case item.Typ == itemBraceRight:
			p.namespaces = append(p.namespaces, p.namespace)
			return
		case item.Val == "related":
			p.parseRelated()
		case item.Val == "permits":
			p.parsePermits()
		case item.Typ == itemOperatorSemicolon:
			continue
		default:
			p.addFatal(item, "expected 'permits' or 'related', got %q", item.Val)
			return
		}
	}
}

func (p *parser) parseRelated() {
	p.match(":", "{")
	for !p.fatal {
		switch item := p.next(); item.Typ {
		case itemBraceRight:
			return
		case itemIdentifier:
			relation := item.Val
			var types []ast.RelationType
			p.match(":")
			switch item := p.next(); item.Typ {
			case itemIdentifier:
				if item.Val == "SubjectSet" {
					types = append(types, p.matchSubjectSet())
				} else {
					types = append(types, ast.RelationType{Namespace: item.Val})
					p.addCheck(checkNamespaceExists(item))
				}
			case itemParenLeft:
				types = append(types, p.parseTypeUnion()...)
			}
			p.match("[", "]", optional(","))
			p.namespace.Relations = append(p.namespace.Relations, ast.Relation{
				Name:  relation,
				Types: types,
			})
		default:
			p.addFatal(item, "expected identifier or '}', got %q", item.Val)
			return
		}
	}
}

func (p *parser) matchSubjectSet() ast.RelationType {
	var namespace, relation item
	p.match("<", &namespace, ",", &relation, ">")
	p.addCheck(checkNamespaceHasRelation(namespace, relation))
	return ast.RelationType{Namespace: namespace.Val, Relation: relation.Val}
}

func (p *parser) parseTypeUnion() (types []ast.RelationType) {
	for !p.fatal {
		var identifier item
		p.match(&identifier)
		if identifier.Val == "SubjectSet" {
			types = append(types, p.matchSubjectSet())
		} else {
			types = append(types, ast.RelationType{Namespace: identifier.Val})
			p.addCheck(checkNamespaceExists(identifier))
		}
		switch item := p.next(); item.Typ {
		case itemParenRight:
			return
		case itemTypeUnion:
		default:
			p.addFatal(item, "expected '|', got %q", item.Val)
		}
	}
	return
}

func (p *parser) parsePermits() {
	p.match("=", "{")
	for !p.fatal {
		switch item := p.next(); item.Typ {

		case itemBraceRight:
			return

		case itemIdentifier:
			permission := item.Val
			p.match(
				":", "(", "ctx", optional(":", "Context"), ")",
				optional(":", "boolean"), "=>",
			)

			rewrite := simplifyExpression(p.parsePermissionExpressions(itemOperatorComma, expressionNestingMaxDepth))
			if rewrite == nil {
				return
			}
			p.namespace.Relations = append(p.namespace.Relations,
				ast.Relation{
					Name:              permission,
					SubjectSetRewrite: rewrite,
				})

		default:
			p.addFatal(item, "expected identifier or '}', got %q", item.Val)
			return
		}
	}
}

func (p *parser) parsePermissionExpressions(finalToken itemType, depth int) *ast.SubjectSetRewrite {
	if depth <= 0 {
		p.addFatal(p.peek(),
			"expression nested too deeply; maximal nesting depth is %d",
			expressionNestingMaxDepth)
		return nil
	}
	var root *ast.SubjectSetRewrite

	// We only expect an expression in the beginning and after a binary
	// operator.
	expectExpression := true

	// TODO(hperl): Split this into two state machines: One that parses an
	// expression or expression group; and one that parses a binary operator.
	for !p.fatal {
		switch item := p.peek(); {

		// A "(" starts a new expression group that is parsed recursively.
		case item.Typ == itemParenLeft:
			p.next() // consume paren
			child := p.parsePermissionExpressions(itemParenRight, depth-1)
			if child == nil {
				return nil
			}
			root = addChild(root, child)
			expectExpression = false

		case item.Typ == finalToken:
			p.next() // consume final token
			return root

		case item.Typ == itemBraceRight:
			// We don't consume the '}' here, to allow `parsePermits` to consume
			// it.
			return root

		case item.Typ == itemOperatorAnd, item.Typ == itemOperatorOr:
			p.next() // consume operator
			newRoot := &ast.SubjectSetRewrite{
				Operation: setOperation(item.Typ),
				Children:  []ast.Child{root},
			}
			root = newRoot
			expectExpression = true

		// A "not" creates an AST node where the children are either a
		// single expression, or a list of expressions grouped by "()".
		case item.Typ == itemOperatorNot:
			p.next() // consume operator
			child := p.parseNotExpression(depth - 1)
			if child == nil {
				return nil
			}
			root = addChild(root, child)
			expectExpression = false

		default:
			if !expectExpression {
				// Two expressions can't follow each other directly, they must
				// be separated by a binary operator.
				p.addFatal(item, "did not expect another expression")
				return nil
			}
			child := p.parsePermissionExpression()
			if child == nil {
				return nil
			}
			root = addChild(root, child)
			expectExpression = true
		}
	}
	return nil
}

func (p *parser) parseNotExpression(depth int) ast.Child {
	if depth <= 0 {
		p.addFatal(p.peek(),
			"expression nested too deeply; maximal nesting depth is %d",
			expressionNestingMaxDepth)
		return nil
	}

	var child ast.Child
	if item := p.peek(); item.Typ == itemParenLeft {
		p.next() // consume paren
		child = p.parsePermissionExpressions(itemParenRight, depth-1)
	} else {
		child = p.parsePermissionExpression()
	}
	if child == nil {
		return nil
	}
	return &ast.InvertResult{Child: child}
}

func addChild(root *ast.SubjectSetRewrite, child ast.Child) *ast.SubjectSetRewrite {
	if root == nil {
		return child.AsRewrite()
	} else {
		root.Children = append(root.Children, child)
		return root
	}
}

func setOperation(typ itemType) ast.Operator {
	switch typ {
	case itemOperatorAnd:
		return ast.OperatorAnd
	case itemOperatorOr:
		return ast.OperatorOr
	}
	panic("not reached")
}

func (p *parser) parsePermissionExpression() (child ast.Child) {
	var name, verb item

	if !p.match("this", ".", &verb, ".", &name) {
		return
	}

	switch verb.Val {
	case "related":
		if !p.match(".") {
			return
		}
		switch item := p.next(); item.Val {
		case "traverse":
			child = p.parseTupleToSubjectSet(name)
		case "includes":
			child = p.parseComputedSubjectSet(name)
		default:
			p.addFatal(item, "expected 'traverse' or 'includes', got %q", item.Val)
		}

	case "permits":
		if !p.match("(", "ctx", ")") {
			return
		}
		p.addCheck(checkCurrentNamespaceHasRelation(&p.namespace, name))
		return &ast.ComputedSubjectSet{Relation: name.Val}

	default:
		p.addFatal(verb, "expected 'related' or 'permits', got %q", verb.Val)

	}

	return
}

func (p *parser) parseTupleToSubjectSet(relation item) (rewrite ast.Child) {
	var (
		subjectSetRel string
		arg, verb     item
	)
	if !p.match("(") {
		return nil
	}

	switch {
	case p.matchIf(is(itemParenLeft), "(", &arg, ")"):
	case p.match(&arg):
	default:
		return nil
	}
	p.match("=>", arg.Val, ".", &verb)

	switch verb.Val {
	case "related":
		p.match(
			".", &subjectSetRel, ".", "includes", "(", "ctx", ".", "subject",
			optional(","), ")", optional(","), ")",
		)
		p.addCheck(checkAllRelationsTypesHaveRelation(
			&p.namespace, relation, subjectSetRel,
		))
	case "permits":
		p.match(".", &subjectSetRel, "(", "ctx", ")", ")")
		p.addCheck(checkAllRelationsTypesHaveRelation(
			&p.namespace, relation, subjectSetRel,
		))
	default:
		p.addFatal(verb, "expected 'related' or 'permits', got %q", verb)
		return nil
	}
	p.addCheck(checkCurrentNamespaceHasRelation(&p.namespace, relation))
	return &ast.TupleToSubjectSet{
		Relation:                   relation.Val,
		ComputedSubjectSetRelation: subjectSetRel,
	}
}

func (p *parser) parseComputedSubjectSet(relation item) (rewrite ast.Child) {
	if !p.match("(", "ctx", ".", "subject", ")") {
		return nil
	}
	p.addCheck(checkCurrentNamespaceHasRelation(&p.namespace, relation))
	return &ast.ComputedSubjectSet{Relation: relation.Val}
}

// simplifyExpression rewrites the expression to use n-ary set operations
// instead of binary ones.
func simplifyExpression(root *ast.SubjectSetRewrite) *ast.SubjectSetRewrite {
	if root == nil {
		return nil
	}
	var newChildren []ast.Child
	for _, child := range root.Children {
		if ch, ok := child.(*ast.SubjectSetRewrite); ok && ch.Operation == root.Operation {
			// merge child and root
			simplifyExpression(ch)
			newChildren = append(newChildren, ch.Children...)
		} else {
			// can't merge, just copy
			newChildren = append(newChildren, child)
		}
	}
	root.Children = newChildren

	return root
}
