// Package evaluator contains the core of our interpreter, which walks
// the AST produced by the parser and evaluates user code.
package evaluator

import (
	"context"
	"fmt"

	"github.com/cloudcmds/tamarin/core/ast"
	"github.com/cloudcmds/tamarin/core/object"
	"github.com/cloudcmds/tamarin/core/scope"
	"github.com/cloudcmds/tamarin/core/stack"
	"github.com/cloudcmds/tamarin/core/token"
)

type Breakpoint struct {
	// File is the file name of the breakpoint
	File string
	// Line is the line number of the breakpoint
	Line int
	// Disabled is true if the breakpoint is disabled.
	Disabled bool
	// Trace is true if the breakpoint should print a trace of the stack when
	// it is hit.
	Trace bool
	// Stop is true if the breakpoint should stop code execution when it is hit.
	Stop bool
}

// Opts configures Tamarin code evaluation.
type Opts struct {
	// Importer is used to import Tamarin code modules. If nil, module imports
	// are not supported and an import will result in an error that stops code
	// execution.
	Importer Importer

	// If set to true, the default builtins will not be registered.
	DisableDefaultBuiltins bool

	// Supplies extra and/or override builtins for evaluation.
	Builtins []*object.Builtin

	// Breakpoints for debugging
	Breakpoints []Breakpoint
}

// Evaluator is used to execute Tamarin AST nodes.
type Evaluator struct {
	importer    Importer
	builtins    map[string]*object.Builtin
	stack       *stack.Stack
	breakpoints map[string]*Breakpoint
}

// New returns a new Evaluator
func New(opts Opts) *Evaluator {
	e := &Evaluator{
		importer:    opts.Importer,
		builtins:    map[string]*object.Builtin{},
		stack:       stack.New(),
		breakpoints: map[string]*Breakpoint{},
	}
	// Conditionally register default global builtins
	if !opts.DisableDefaultBuiltins {
		for _, b := range GlobalBuiltins() {
			e.builtins[b.Key()] = b
		}
	}
	// Add override builtins
	for _, b := range opts.Builtins {
		e.builtins[b.Key()] = b
	}
	// Index breakpoints by "file:line"
	for _, b := range opts.Breakpoints {
		e.breakpoints[fmt.Sprintf("%s:%d", b.File, b.Line)] = &b
	}
	return e
}

// Returns a function that implements object.CallFunc
func (e *Evaluator) getCallFunc() object.CallFunc {
	return func(ctx context.Context, s interface{}, fn object.Object, args []object.Object) object.Object {
		var scopeObj *scope.Scope
		if s != nil {
			scopeObj = s.(*scope.Scope)
		}
		return e.applyFunction(ctx, scopeObj, fn, args)
	}
}

func (e *Evaluator) GetBreakpoint(tok token.Token) (*Breakpoint, bool) {
	if len(e.breakpoints) == 0 {
		return nil, false
	}
	b, found := e.breakpoints[fmt.Sprintf("%s:%d",
		tok.StartPosition.File,
		tok.StartPosition.LineNumber())]
	return b, found
}

func (e *Evaluator) trackExecution(statement ast.Statement, s *scope.Scope) object.Object {
	e.stack.TrackStatement(statement, s)
	tok := statement.Token()
	if b, found := e.GetBreakpoint(tok); found && !b.Disabled {
		location := fmt.Sprintf("%s:%d", tok.StartPosition.File, tok.StartPosition.LineNumber())
		fmt.Println("----------------")
		fmt.Printf("breakpoint @ %s\n\n", location)
		if b.Trace {
			fmt.Println("trace:")
			fmt.Println(e.stack.String())
			fmt.Println()
		}
		if b.Stop {
			frame := e.stack.Top()
			fmt.Println("locals:")
			contents := frame.Scope().Contents()
			for _, k := range frame.Scope().Keys() {
				fmt.Printf("%s = %s\n", k, contents[k])
			}
			fmt.Println()
			fmt.Println("enter to continue")
			var resp string
			fmt.Scanln(&resp)
			fmt.Println("continuing...")
			fmt.Println()
		}
	}
	return nil
}

// Evaluate an AST node. The context can be used to cancel a running evaluation.
// If evaluation encounters an error, a Tamarin error object is returned.
func (e *Evaluator) Evaluate(ctx context.Context, node ast.Node, s *scope.Scope) object.Object {

	// Add an object.CallFunc to the context so that objects can call Tamarin
	// functions if needed
	ctx = object.WithCallFunc(ctx, e.getCallFunc())

	// Check for context timeout
	select {
	case <-ctx.Done():
		return object.NewError(ctx.Err())
	default:
	}

	// Track statement execution for tracing
	if statement, ok := node.(ast.Statement); ok {
		if result := e.trackExecution(statement, s); result != nil {
			return result
		}
	}

	// Evaluate the AST node based on its type
	switch node := node.(type) {

	// High level types
	case *ast.Program:
		return e.evalProgram(ctx, node, s)
	case *ast.Block:
		return e.evalBlockStatement(ctx, node, s)

	// Operator expressions
	case *ast.Prefix:
		return e.evalPrefixExpression(ctx, node, s)
	case *ast.Postfix:
		return e.evalPostfixExpression(s, node.Operator(), node)
	case *ast.Infix:
		return e.evalInfixExpression(ctx, node, s)
	case *ast.Ternary:
		return e.evalTernaryExpression(ctx, node, s)
	case *ast.In:
		return e.evalIn(ctx, node, s)

	// Miscellaneous
	case *ast.Ident:
		return e.evalIdentifier(node, s)
	case *ast.Index:
		return e.evalIndexExpression(ctx, node, s)
	case *ast.Slice:
		return e.evalSliceExpression(ctx, node, s)
	case *ast.Bool:
		return nativeBoolToBooleanObject(node.Value())
	case *ast.Import:
		return e.evalImportStatement(ctx, node, s)

	// Assignment
	case *ast.Var:
		return e.evalVarStatement(ctx, node, s)
	case *ast.Const:
		return e.evalConstStatement(ctx, node, s)
	case *ast.Assign:
		return e.evalAssignStatement(ctx, node, s)
	case *ast.MultiVar:
		return e.evalMultiVarStatement(ctx, node, s)

	// Functions
	case *ast.Func:
		return e.evalFunctionLiteral(ctx, node, s)

	// Calls
	case *ast.ObjectCall:
		return e.evalObjectCallExpression(ctx, node, s)
	case *ast.Call:
		return e.evalCallExpression(ctx, node, s)
	case *ast.GetAttr:
		return e.evalGetAttributeExpression(ctx, node, s)

	// Control
	case *ast.If:
		return e.evalIf(ctx, node, s)
	case *ast.For:
		return e.evalFor(ctx, node, s)
	case *ast.Switch:
		return e.evalSwitch(ctx, node, s)
	case *ast.Pipe:
		return e.evalPipe(ctx, node, s)
	case *ast.Control:
		return e.evalControl(ctx, node, s)
	case *ast.Range:
		return e.evalRange(ctx, node, s)

	// Literals
	case *ast.Nil:
		return object.Nil
	case *ast.Int:
		return object.NewInt(node.Value())
	case *ast.Float:
		return object.NewFloat(node.Value())
	case *ast.String:
		return e.evalStringLiteral(ctx, node, s)
	case *ast.List:
		return e.evalListLiteral(ctx, node, s)
	case *ast.Map:
		return e.evalMapLiteral(ctx, node, s)
	case *ast.Set:
		return e.evalSetLiteral(ctx, node, s)
	}

	panic(fmt.Sprintf("unknown ast node type: %T", node))
}
